package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"locless-vhd/proto"
	"locless-vhd/server/store"
)

// fakeSaveStore is a store with a deploy into VHDX, in Save Mode or not.
type fakeSaveStore struct {
	state    proto.StoreState
	disk     *fakeDisk
	entered  int
	left     int
	accounts *proto.Accounts
	closed   bool
	opened   bool
}

func (s *fakeSaveStore) State() proto.StoreState { return s.state }

func (s *fakeSaveStore) EnterSave() error {
	s.entered++
	s.state.Save = proto.SaveFile
	return nil
}

func (s *fakeSaveStore) LeaveSave(a *proto.Accounts) error {
	s.left++
	s.accounts = a
	s.state.Save = ""
	return nil
}

func (s *fakeSaveStore) OpenSaved() (source, error) {
	s.opened = true
	return s.disk, nil
}

func (s *fakeSaveStore) Close() error {
	s.closed = true
	return nil
}

func savingStore(image string, disk *fakeDisk) *fakeSaveStore {
	return &fakeSaveStore{
		state: proto.StoreState{Image: image, Version: 1, Format: "vhdx", Base: proto.BaseFile,
			Local: proto.LocalFile, Save: proto.SaveFile},
		disk: disk,
	}
}

func mustMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC(s)
	if err != nil {
		t.Fatal(err)
	}
	return mac
}

// runSave runs one task of Save Mode against the real server.
func (e *cloneEnv) runSave(t *testing.T, kind, image string, s *fakeSaveStore) (*worker, error) {
	t.Helper()
	if _, err := e.s.QueueTask(context.Background(), e.host.ID, kind, image); err != nil {
		t.Fatalf("QueueTask: %v", err)
	}
	w, _ := testWorker(e.url)
	w.openStore = func() (savedStore, error) { return s, nil }
	w.openDisk = func() (source, error) { return nil, errors.New("Save Mode must not read the physical disk") }
	return w, w.attempt()
}

// TestSaveImageMakesAVersionAndQueuesTheDeploy: the disk of Save Mode becomes the
// next version, and the server sends the PC the deploy of it.
func TestSaveImageMakesAVersionAndQueuesTheDeploy(t *testing.T) {
	e := newCloneEnv(t)
	size := uint64(40 * testBlock)
	blocks := map[uint32][]byte{}
	for pos := range uint32(30) {
		blocks[pos] = fill(byte(pos), testBlock)
	}
	e.run(t, &fakeDisk{blocks: blocks, size: size}) // v1 from a master PC

	saved := map[uint32][]byte{}
	for pos, raw := range blocks {
		saved[pos] = raw
	}
	saved[3] = fill(201, testBlock) // what the operator installed
	saved[35] = fill(202, testBlock)
	disk := &fakeDisk{blocks: saved, size: size}
	s := savingStore("win11", disk)
	w, err := e.runSave(t, store.TaskSaveImage, "win11", s)
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	sameBlocks(t, e.restore(t, "v1.ldisk", "v2.ldelta"), saved)
	if !disk.shrunk || !disk.restored || !disk.closed || !s.closed {
		t.Fatalf("disk %+v, store %+v", disk, s)
	}
	if st := w.screen.snapshot(); st.Title != "Готово" || !strings.Contains(st.Detail, "v2.ldelta") {
		t.Fatalf("screen: %+v", st)
	}

	ctx := context.Background()
	// The table of a virtual disk must never wait on the server: the next task
	// would apply it to the physical disk.
	if table, _ := e.s.SavedTable(ctx, mustMAC(t, e.host.MAC)); table != nil {
		t.Fatalf("table saved on the server: %q", table)
	}
	next, err := e.s.ActiveTask(ctx, mustMAC(t, e.host.MAC))
	if err != nil || next == nil || next.Kind != store.TaskDeployVHD || next.Image != "win11" {
		t.Fatalf("next task %+v, err %v", next, err)
	}
	if hosts, _ := e.s.Hosts(ctx); hosts[0].LastTask == nil || hosts[0].LastTask.Kind != store.TaskSaveImage {
		t.Fatalf("host: %+v", hosts[0])
	}
}

func TestSaveImageNeedsSaveMode(t *testing.T) {
	e := newCloneEnv(t)
	s := savingStore("win11", &fakeDisk{blocks: map[uint32][]byte{0: fill(1, testBlock)}, size: testBlock})
	s.state.Save = ""
	if _, err := e.runSave(t, store.TaskSaveImage, "win11", s); !errors.Is(err, errNotSaving) {
		t.Fatalf("got %v", err)
	}
	if s.opened {
		t.Fatal("the disk was attached outside Save Mode")
	}
	if images, _ := e.s.Images(context.Background()); len(images) != 0 {
		t.Fatalf("a version was made: %+v", images)
	}
}

// TestSaveImageOfAnotherImage: the PC holds win10, the panel says win11.
func TestSaveImageOfAnotherImage(t *testing.T) {
	e := newCloneEnv(t)
	s := savingStore("win10", &fakeDisk{blocks: map[uint32][]byte{0: fill(1, testBlock)}, size: testBlock})
	if _, err := e.runSave(t, store.TaskSaveImage, "win11", s); err == nil || !strings.Contains(err.Error(), "win10") {
		t.Fatalf("got %v", err)
	}
	if s.opened {
		t.Fatal("the disk was attached for the wrong image")
	}
}

func TestSaveModeNeedsADeployIntoVHDX(t *testing.T) {
	e := newCloneEnv(t)
	s := savingStore("win11", nil)
	s.state = proto.StoreState{}
	if _, err := e.runSave(t, store.TaskSaveMode, "", s); err == nil || s.entered != 0 {
		t.Fatalf("err %v, entered %d", err, s.entered)
	}
}

func TestSaveModeEntersOnce(t *testing.T) {
	e := newCloneEnv(t)
	s := savingStore("win11", nil)
	s.state.Save = ""
	for range 2 {
		if _, err := e.runSave(t, store.TaskSaveMode, "", s); err != nil {
			t.Fatalf("attempt: %v", err)
		}
	}
	if s.entered != 1 || s.state.Save != proto.SaveFile {
		t.Fatalf("entered %d, state %+v", s.entered, s.state)
	}
	if hosts, _ := e.s.Hosts(context.Background()); !hosts[0].SaveMode {
		t.Fatalf("host: %+v", hosts[0])
	}
}

// TestSaveDiscardSetsUpThePlayers: the chain of the players comes back with the
// accounts of the panel.
func TestSaveDiscardSetsUpThePlayers(t *testing.T) {
	e := newCloneEnv(t)
	ctx := context.Background()
	if err := e.s.SetSetting(ctx, store.SettingPlayerUser, "Player"); err != nil {
		t.Fatal(err)
	}
	s := savingStore("win11", nil)
	if _, err := e.runSave(t, store.TaskSaveDiscard, "", s); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if s.left != 1 || s.accounts == nil || s.accounts.PlayerUser != "Player" {
		t.Fatalf("left %d, accounts %+v", s.left, s.accounts)
	}
	// Not in Save Mode: nothing to do, and still a success.
	if _, err := e.runSave(t, store.TaskSaveDiscard, "", s); err != nil || s.left != 1 {
		t.Fatalf("second: %v, left %d", err, s.left)
	}
}
