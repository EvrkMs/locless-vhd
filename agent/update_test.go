package main

import (
	"bytes"
	"maps"
	"strings"
	"testing"

	"locless-vhd/proto"
	"locless-vhd/server/store"
)

// updatingVHD is a deploy into VHDX onto a store that may hold an earlier version.
type updatingVHD struct {
	*fakeVHD
	held     *proto.BaseIndex
	wiped    bool // Prepare ran: a whole deploy
	reopened bool
	saved    *proto.BaseIndex
}

func newUpdatingVHD(blocks map[uint64][]byte, held *proto.BaseIndex) *updatingVHD {
	return &updatingVHD{fakeVHD: &fakeVHD{fakeTarget: &fakeTarget{blocks: blocks}}, held: held}
}

func (v *updatingVHD) Prepare(m proto.Manifest) error {
	v.wiped = true
	return v.fakeTarget.Prepare(m)
}

func (v *updatingVHD) Held(image string) (proto.BaseIndex, bool) {
	if v.held == nil || v.held.Image != image {
		return proto.BaseIndex{}, false
	}
	return *v.held, true
}

func (v *updatingVHD) Reopen(proto.Manifest) error {
	v.reopened, v.prepared = true, true
	return nil
}

func (v *updatingVHD) Remember(x proto.BaseIndex) error {
	v.saved = &x
	return nil
}

// deployVHDTarget runs a deploy into VHDX onto v.
func (e *cloneEnv) deployVHDTarget(t *testing.T, v vhdTarget) error {
	t.Helper()
	if _, err := e.s.QueueTask(t.Context(), e.host.ID, store.TaskDeployVHD, "win11"); err != nil {
		t.Fatalf("QueueTask: %v", err)
	}
	w, _ := testWorker(e.url)
	w.openVHD = func(uint64, uint64) (vhdTarget, error) { return v, nil }
	return w.attempt()
}

// TestUpdateWritesOnlyTheChanges: the second deploy finds v1 in the base and
// writes only what v2 changed; the result is v2 all the same.
func TestUpdateWritesOnlyTheChanges(t *testing.T) {
	e := newCloneEnv(t)
	size := uint64(12 * testBlock)
	v1 := map[uint32][]byte{}
	for pos := range uint32(8) {
		v1[pos] = fill(byte(pos), testBlock)
	}
	e.run(t, &fakeDisk{blocks: v1, size: size})

	first := newUpdatingVHD(map[uint64][]byte{}, nil)
	if err := e.deployVHDTarget(t, first); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	if !first.wiped || first.reopened || first.saved == nil || first.saved.Version != 1 || len(first.saved.Entries) != len(v1) {
		t.Fatalf("first: wiped %v, reopened %v, saved %+v", first.wiped, first.reopened, first.saved)
	}

	v2 := maps.Clone(v1)
	v2[3] = fill(203, testBlock)    // changed
	v2[4] = make([]byte, testBlock) // used and all zeros now
	delete(v2, 5)                   // free now: left as it is
	v2[9] = fill(209, testBlock)    // new
	e.run(t, &fakeDisk{blocks: v2, size: size})

	second := newUpdatingVHD(maps.Clone(first.blocks), first.saved)
	if err := e.deployVHDTarget(t, second); err != nil {
		t.Fatalf("second deploy: %v", err)
	}
	if second.wiped || !second.reopened {
		t.Fatalf("wiped %v, reopened %v", second.wiped, second.reopened)
	}
	if second.writes != 3 {
		t.Fatalf("%d writes, want 3: the changed, the zeroed and the new block", second.writes)
	}
	for pos, raw := range v2 {
		if !bytes.Equal(second.blocks[uint64(pos)*testBlock], raw) {
			t.Fatalf("block %d differs", pos)
		}
	}
	if !bytes.Equal(second.blocks[5*testBlock], v1[5]) {
		t.Fatal("a block that became free was touched")
	}
	if second.saved == nil || second.saved.Version != 2 || second.state == nil || second.state.Version != 2 {
		t.Fatalf("saved %+v, state %+v", second.saved, second.state)
	}
	hosts, _ := e.s.Hosts(t.Context())
	if msg := hosts[0].LastTask.Message; !strings.Contains(msg, "v1 → v2") || !strings.Contains(msg, "изменилось 3 блоков") {
		t.Fatalf("message %q", msg)
	}
}

// TestUpdateFallsBackToAWholeDeploy: whatever does not match is written whole.
func TestUpdateFallsBackToAWholeDeploy(t *testing.T) {
	e := newCloneEnv(t)
	e.run(t, &fakeDisk{blocks: map[uint32][]byte{0: fill(1, testBlock)}, size: 4 * testBlock})
	first := newUpdatingVHD(map[uint64][]byte{}, nil)
	if err := e.deployVHDTarget(t, first); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	held := *first.saved

	cases := map[string]func(x *proto.BaseIndex){
		"another image":     func(x *proto.BaseIndex) { x.Image = "win10" },
		"another table":     func(x *proto.BaseIndex) { x.Manifest.DiskSize += 1 << 30 },
		"another blocksize": func(x *proto.BaseIndex) { x.BlockSize *= 2 },
	}
	for name, change := range cases {
		x := held
		x.Manifest.Partitions = append([]proto.Partition(nil), held.Manifest.Partitions...)
		change(&x)
		v := newUpdatingVHD(map[uint64][]byte{}, &x)
		if err := e.deployVHDTarget(t, v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !v.wiped || v.reopened {
			t.Fatalf("%s: wiped %v, reopened %v", name, v.wiped, v.reopened)
		}
	}
}
