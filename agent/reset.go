package main

import (
	"errors"
	"fmt"

	"locless-vhd/proto"
)

// The reset runs as a service of the Windows booted from a differencing disk.
// Windows writes into the disk it booted from, so that one cannot be reset now.
// Instead the service makes the other disk of the pair fresh and points the boot
// entry at it: the next boot starts clean, and the disk of this session is
// thrown away at the start of the one after.
//
//	deploy:  diff-a fresh, boot → a
//	boot 1:  on a; delete b, make b, boot → b
//	boot 2:  on b; delete a (session 1), make a, boot → a
//
// The boot entry changes only after the new disk is complete, so a failure or a
// power cut leaves the PC booting the disk it booted last.

// errInUse means the file is held open: Windows runs from it.
var errInUse = errors.New("файл занят")

// resetOps is what the reset does to the store and the boot entry.
type resetOps interface {
	ReadState() (proto.StoreState, error)
	WriteState(proto.StoreState) error
	// Remove deletes a file of the store; a missing one is fine. A file Windows
	// runs from gives errInUse.
	Remove(name string) error
	// CreateDiff makes an empty differencing disk on top of parent.
	CreateDiff(name, parent string) error
	// Point makes the boot entry Windows booted from use this differencing disk.
	Point(name string) error
	// ReadAccounts returns what the deploy asked of the first boot, nil if nothing.
	ReadAccounts() (*proto.Accounts, error)
	RemoveAccounts() error
	// ApplyAccounts changes the users of the running Windows.
	ApplyAccounts(*proto.Accounts) error
	// Reboot restarts the PC once the service is done.
	Reboot() error
}

// startup is what the service does when Windows starts. The first boot after a
// deploy with accounts runs from local itself: the accounts are set up there, so
// they stay, then the first differencing disk is made and the PC restarts into it.
func startup(ops resetOps) (string, error) {
	accounts, err := ops.ReadAccounts()
	if err != nil {
		return "", fmt.Errorf("файл учётных записей: %w", err)
	}
	if accounts == nil {
		return resetDiff(ops)
	}
	applied := ops.ApplyAccounts(accounts)
	// The file holds a password: it goes whatever happened, and a failure is only
	// logged. Trying again on the next boot would write into a throwaway disk.
	removed := ops.RemoveAccounts()
	said, err := resetDiff(ops)
	if err != nil {
		return "", errors.Join(applied, removed, err)
	}
	if err := ops.Reboot(); err != nil {
		return "", errors.Join(applied, removed, fmt.Errorf("перезагрузка: %w", err))
	}
	if err := errors.Join(applied, removed); err != nil {
		return "учётные записи с ошибкой, " + said, err
	}
	return "учётные записи настроены, " + said + ", перезагрузка", nil
}

// resetDiff prepares the next boot and says what it did.
func resetDiff(ops resetOps) (string, error) {
	st, err := ops.ReadState()
	if err != nil {
		return "", fmt.Errorf("файл состояния: %w", err)
	}
	if st.Base == "" {
		return "", errors.New("в файле состояния нет base: это не деплой в VHDX")
	}
	booted := st.Diff
	if booted != proto.DiffFile && booted != proto.DiffFileB {
		booted = proto.DiffFile
	}
	next := proto.OtherDiff(booted)
	err = ops.Remove(next)
	if errors.Is(err, errInUse) {
		// The state is behind the boot entry: the last reset switched the entry and
		// went down before it wrote the state. Windows runs from the other disk.
		booted, next = next, booted
		err = ops.Remove(next)
	}
	if err != nil {
		return "", fmt.Errorf("удаление %s: %w", next, err)
	}
	parent := st.Local
	if parent == "" {
		parent = st.Base // a deploy from before local.avhdx
	}
	if err := ops.CreateDiff(next, parent); err != nil {
		return "", fmt.Errorf("создание %s: %w", next, err)
	}
	if err := ops.Point(next); err != nil {
		return "", fmt.Errorf("загрузочная запись на %s: %w", next, err)
	}
	st.Diff = next
	if err := ops.WriteState(st); err != nil {
		return "", fmt.Errorf("файл состояния: %w", err)
	}
	return fmt.Sprintf("работает %s, следующая загрузка с чистого %s", booted, next), nil
}
