package main

import (
	"errors"
	"strings"
	"testing"

	"locless-vhd/proto"
)

// fakeStore is the store with Windows running from one differencing disk.
type fakeStore struct {
	state    proto.StoreState
	files    map[string]int // name → generation, so a remade disk is told from the old one
	running  string
	pointed  string
	failMake bool
	steps    []string
	parents  []string
	accounts *proto.Accounts
	applied  *proto.Accounts
	applyErr error
	rebooted bool
}

func (s *fakeStore) ReadAccounts() (*proto.Accounts, error) { return s.accounts, nil }
func (s *fakeStore) RemoveAccounts() error {
	s.steps = append(s.steps, "remove accounts")
	s.accounts = nil
	return nil
}

func (s *fakeStore) ApplyAccounts(a *proto.Accounts) error {
	s.steps = append(s.steps, "accounts")
	s.applied = a
	return s.applyErr
}

func (s *fakeStore) Reboot() error {
	s.steps = append(s.steps, "reboot")
	s.rebooted = true
	return nil
}

func newFakeStore(running string, stateDiff string) *fakeStore {
	return &fakeStore{
		state:   proto.StoreState{Image: "win11", Version: 3, Base: proto.BaseFile, Local: proto.LocalFile, Diff: stateDiff},
		files:   map[string]int{proto.BaseFile: 1, proto.LocalFile: 1, proto.DiffFile: 1, proto.DiffFileB: 1},
		running: running, pointed: running,
	}
}

func (s *fakeStore) ReadState() (proto.StoreState, error) { return s.state, nil }
func (s *fakeStore) WriteState(st proto.StoreState) error {
	s.steps = append(s.steps, "state "+st.Diff)
	s.state = st
	return nil
}

func (s *fakeStore) Remove(name string) error {
	if name == s.running || name == proto.BaseFile || name == proto.LocalFile {
		return errInUse
	}
	s.steps = append(s.steps, "remove "+name)
	delete(s.files, name)
	return nil
}

func (s *fakeStore) CreateDiff(name, parent string) error {
	s.parents = append(s.parents, parent)
	if s.failMake {
		return errors.New("no space")
	}
	if _, ok := s.files[name]; ok {
		return errors.New("file exists")
	}
	s.steps = append(s.steps, "create "+name)
	s.files[name] = 2
	return nil
}

func (s *fakeStore) Point(name string) error {
	s.steps = append(s.steps, "point "+name)
	s.pointed = name
	return nil
}

func TestResetPreparesTheOtherDisk(t *testing.T) {
	for _, running := range []string{proto.DiffFile, proto.DiffFileB} {
		s := newFakeStore(running, running)
		said, err := resetDiff(s)
		if err != nil {
			t.Fatalf("%s: %v", running, err)
		}
		next := proto.OtherDiff(running)
		want := "remove " + next + ",create " + next + ",point " + next + ",state " + next
		if got := strings.Join(s.steps, ","); got != want {
			t.Fatalf("%s: steps %s, want %s", running, got, want)
		}
		if s.files[next] != 2 || s.files[running] != 1 || !strings.Contains(said, next) || s.parents[0] != proto.LocalFile {
			t.Fatalf("%s: files %v, said %q", running, s.files, said)
		}
	}
}

// TestResetCatchesUpWithTheBootEntry: the last reset pointed the entry at b and
// stopped before the state said so. Windows now runs from b; a must be remade.
func TestResetCatchesUpWithTheBootEntry(t *testing.T) {
	s := newFakeStore(proto.DiffFileB, proto.DiffFile)
	if _, err := resetDiff(s); err != nil {
		t.Fatalf("resetDiff: %v", err)
	}
	if s.pointed != proto.DiffFile || s.state.Diff != proto.DiffFile || s.files[proto.DiffFile] != 2 {
		t.Fatalf("pointed %s, state %s, files %v", s.pointed, s.state.Diff, s.files)
	}
}

// TestResetFailureKeepsTheBoot: without a complete new disk the entry is left alone.
func TestResetFailureKeepsTheBoot(t *testing.T) {
	s := newFakeStore(proto.DiffFile, proto.DiffFile)
	s.failMake = true
	if _, err := resetDiff(s); err == nil {
		t.Fatal("no error")
	}
	if s.pointed != proto.DiffFile || s.state.Diff != proto.DiffFile {
		t.Fatalf("pointed %s, state %s", s.pointed, s.state.Diff)
	}
}

func TestResetNeedsAVHDXDeploy(t *testing.T) {
	s := newFakeStore(proto.DiffFile, proto.DiffFile)
	s.state.Base = ""
	if _, err := resetDiff(s); err == nil || len(s.steps) != 0 {
		t.Fatalf("err %v, steps %v", err, s.steps)
	}
}

// TestResetOfAnOldDeploy: a store from before local.vhdx keeps its diffs on the base.
func TestResetOfAnOldDeploy(t *testing.T) {
	s := newFakeStore(proto.DiffFile, proto.DiffFile)
	s.state.Local = ""
	if _, err := resetDiff(s); err != nil {
		t.Fatalf("resetDiff: %v", err)
	}
	if len(s.parents) != 1 || s.parents[0] != proto.BaseFile {
		t.Fatalf("parents %v", s.parents)
	}
}

// TestFirstBootSetsUpAccounts: Windows runs from local, the deploy made no
// differencing disk yet. The accounts come first, the password file goes, then the
// first differencing disk, and the PC restarts into it.
func TestFirstBootSetsUpAccounts(t *testing.T) {
	s := newFakeStore(proto.LocalFile, "")
	delete(s.files, proto.DiffFile)
	delete(s.files, proto.DiffFileB)
	s.accounts = &proto.Accounts{AdminPassword: "secret", PlayerUser: "Player"}
	if _, err := startup(s); err != nil {
		t.Fatalf("startup: %v", err)
	}
	want := "accounts,remove accounts,remove " + proto.DiffFileB + ",create " + proto.DiffFileB +
		",point " + proto.DiffFileB + ",state " + proto.DiffFileB + ",reboot"
	if got := strings.Join(s.steps, ","); got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
	if s.applied == nil || s.applied.PlayerUser != "Player" || s.accounts != nil || s.parents[0] != proto.LocalFile {
		t.Fatalf("applied %+v, file %+v, parents %v", s.applied, s.accounts, s.parents)
	}
}

// TestFirstBootGoesOnAfterAFailure: a failed account change must not leave the PC
// writing into local on every boot.
func TestFirstBootGoesOnAfterAFailure(t *testing.T) {
	s := newFakeStore(proto.LocalFile, "")
	s.accounts = &proto.Accounts{PlayerUser: "Player"}
	s.applyErr = errors.New("access denied")
	if _, err := startup(s); err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("startup: %v", err)
	}
	if !s.rebooted || s.pointed != proto.DiffFileB || s.accounts != nil {
		t.Fatalf("rebooted %v, pointed %s, file %+v", s.rebooted, s.pointed, s.accounts)
	}
}

// TestUsualBootDoesNotRestart: without the file it is the plain reset.
func TestUsualBootDoesNotRestart(t *testing.T) {
	s := newFakeStore(proto.DiffFile, proto.DiffFile)
	if _, err := startup(s); err != nil {
		t.Fatalf("startup: %v", err)
	}
	if s.rebooted || s.applied != nil {
		t.Fatalf("rebooted %v, applied %+v", s.rebooted, s.applied)
	}
}
