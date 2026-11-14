package main

import (
	"context"
	"errors"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
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
	layout   *proto.VHDXLayout // what save.vhdx holds; nil: its BAT cannot be read
}

func (s *fakeSaveStore) Changes() (proto.VHDXLayout, error) {
	if s.layout == nil {
		return proto.VHDXLayout{}, errors.New("BAT не прочитан")
	}
	return *s.layout, nil
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

// savedOver makes v1 on the server from blocks and returns the disk of Save Mode
// over it: position 3 changed and 35 added. Partition 3 of the fake disk starts
// at 32 MiB, and the VHDX blocks are 1 MiB like the image's.
func savedOver(t *testing.T, e *cloneEnv) (map[uint32][]byte, *fakeDisk, *proto.VHDXLayout) {
	t.Helper()
	size := uint64(40 * testBlock)
	blocks := map[uint32][]byte{}
	for pos := range uint32(30) {
		blocks[pos] = fill(byte(pos), testBlock)
	}
	e.run(t, &fakeDisk{blocks: blocks, size: size})
	saved := maps.Clone(blocks)
	saved[3] = fill(203, testBlock)
	saved[35] = fill(235, testBlock)
	layout := &proto.VHDXLayout{BlockSize: testBlock, SectorSize: 512, VirtualSize: 1 << 30, HasParent: true,
		Present: []uint64{32 + 3, 32 + 35, 0}} // block 0 is the GPT: no partition there
	return saved, &fakeDisk{blocks: saved, size: size}, layout
}

// fileID is the ID of an image file on the server, what a deploy of it records.
func (e *cloneEnv) fileID(t *testing.T, name string) string {
	t.Helper()
	f, err := os.Open(filepath.Join(e.images, "win11", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()
	pf, err := proto.OpenFile(f, info.Size())
	if err != nil {
		t.Fatal(err)
	}
	return pf.Header.ID.String()
}

// TestSaveImageOverAReusedNumber: v1 was deleted and remade from another disk.
// The number matches, the file does not, so nothing is inherited.
func TestSaveImageOverAReusedNumber(t *testing.T) {
	e := newCloneEnv(t)
	saved, disk, layout := savedOver(t, e)
	s := savingStore("win11", disk)
	s.layout = layout
	s.state.BaseID = proto.NewID().String() // the file the PC got is gone
	if _, err := e.runSave(t, store.TaskSaveImage, "win11", s); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if len(disk.reads) != 40 {
		t.Fatalf("read %d places, want all 40", len(disk.reads))
	}
	sameBlocks(t, e.restore(t, "v1.ldisk", "v2.ldelta"), saved)
}

// TestSaveImageReadsOnlyTheChanges: the BAT of save.vhdx names the two blocks
// that changed, the server keeps the rest from v1, and v2 is the whole disk.
func TestSaveImageReadsOnlyTheChanges(t *testing.T) {
	e := newCloneEnv(t)
	saved, disk, layout := savedOver(t, e)
	s := savingStore("win11", disk)
	s.layout = layout
	s.state.BaseID = e.fileID(t, "v1.ldisk")
	if _, err := e.runSave(t, store.TaskSaveImage, "win11", s); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	slices.Sort(disk.reads)
	if !slices.Equal(disk.reads, []uint32{3, 35}) {
		t.Fatalf("read %v, want only [3 35]", disk.reads)
	}
	sameBlocks(t, e.restore(t, "v1.ldisk", "v2.ldelta"), saved)
	if !disk.shrunk || !disk.restored {
		t.Fatalf("disk %+v", disk)
	}
}

// TestSaveImageOverAnOlderVersion: another PC made v2 meanwhile, so the base of
// this one is not the newest. Inheriting from v2 would be wrong: all is read.
func TestSaveImageOverAnOlderVersion(t *testing.T) {
	e := newCloneEnv(t)
	saved, disk, layout := savedOver(t, e)
	other := map[uint32][]byte{0: fill(250, testBlock), 1: fill(251, testBlock)}
	e.run(t, &fakeDisk{blocks: other, size: 40 * testBlock}) // v2 from elsewhere

	s := savingStore("win11", disk) // the base is v1
	s.layout = layout
	s.state.BaseID = e.fileID(t, "v1.ldisk")
	if _, err := e.runSave(t, store.TaskSaveImage, "win11", s); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if len(disk.reads) != 40 { // every place of the partition, the free ones too
		t.Fatalf("read %d places, want all 40", len(disk.reads))
	}
	sameBlocks(t, e.restore(t, "v1.ldisk", "v2.ldelta", "v3.ldelta"), saved)
}

// TestSaveImageWithoutTheBAT: the changes are unknown, so all is read.
func TestSaveImageWithoutTheBAT(t *testing.T) {
	e := newCloneEnv(t)
	saved, disk, _ := savedOver(t, e)
	s := savingStore("win11", disk)
	if _, err := e.runSave(t, store.TaskSaveImage, "win11", s); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if len(disk.reads) != 40 { // every place of the partition, the free ones too
		t.Fatalf("read %d places, want all 40", len(disk.reads))
	}
	sameBlocks(t, e.restore(t, "v1.ldisk", "v2.ldelta"), saved)
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
