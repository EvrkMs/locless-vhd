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
}

func (s *fakeStore) ReadAccounts() (*proto.Accounts, error) { return s.accounts, nil }

func (s *fakeStore) ApplyAccounts(a *proto.Accounts) error {
	s.steps = append(s.steps, "accounts")
	s.applied = a
	return s.applyErr
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

// TestAccountsEveryBoot: the accounts come before the reset, the file stays for
// the next boot, and the PC does not restart.
func TestAccountsEveryBoot(t *testing.T) {
	s := newFakeStore(proto.DiffFile, proto.DiffFile)
	s.accounts = &proto.Accounts{AdminPassword: "secret", PlayerUser: "Player"}
	said, err := startup(s)
	if err != nil {
		t.Fatalf("startup: %v", err)
	}
	want := "accounts,remove " + proto.DiffFileB + ",create " + proto.DiffFileB +
		",point " + proto.DiffFileB + ",state " + proto.DiffFileB
	if got := strings.Join(s.steps, ","); got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
	if s.applied == nil || s.applied.PlayerUser != "Player" || s.accounts == nil || !strings.Contains(said, "учётные записи") {
		t.Fatalf("applied %+v, file %+v, said %q", s.applied, s.accounts, said)
	}
}

// TestResetGoesOnAfterAnAccountFailure: the next boot must start clean anyway.
func TestResetGoesOnAfterAnAccountFailure(t *testing.T) {
	s := newFakeStore(proto.DiffFile, proto.DiffFile)
	s.accounts = &proto.Accounts{PlayerUser: "Player"}
	s.applyErr = errors.New("access denied")
	if _, err := startup(s); err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("startup: %v", err)
	}
	if s.pointed != proto.DiffFileB || s.state.Diff != proto.DiffFileB {
		t.Fatalf("pointed %s, state %s", s.pointed, s.state.Diff)
	}
}

// TestSaveModeTouchesNothing: the work of the operator survives the boot, and
// the accounts of the players stay out of the image.
func TestSaveModeTouchesNothing(t *testing.T) {
	s := newFakeStore(proto.SaveFile, proto.DiffFile)
	s.state.Save = proto.SaveFile
	s.accounts = &proto.Accounts{AdminPassword: "secret", PlayerUser: "Player"}
	said, err := startup(s)
	if err != nil {
		t.Fatalf("startup: %v", err)
	}
	if len(s.steps) != 0 || s.applied != nil || !strings.Contains(said, "Save Mode") {
		t.Fatalf("steps %v, applied %+v, said %q", s.steps, s.applied, said)
	}
	if _, err := resetDiff(s); err == nil || len(s.steps) != 0 {
		t.Fatalf("resetDiff in Save Mode: %v, steps %v", err, s.steps)
	}
}

// TestUsualBootWithoutAccounts: without the file it is the plain reset.
func TestUsualBootWithoutAccounts(t *testing.T) {
	s := newFakeStore(proto.DiffFile, proto.DiffFile)
	if _, err := startup(s); err != nil {
		t.Fatalf("startup: %v", err)
	}
	if s.applied != nil || s.pointed != proto.DiffFileB {
		t.Fatalf("applied %+v, pointed %s", s.applied, s.pointed)
	}
}
