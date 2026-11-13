package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"locless-vhd/proto"
)

// resetServiceName is the service the deploy into VHDX installs into the base.
const resetServiceName = "LoclessReset"

// resetLogLimit keeps the log on the store small: it is cut in half when larger.
const resetLogLimit = 1 << 20

// runReset is the agent started by Windows as the reset service, or by hand
// from a console to try it.
func runReset() error {
	service, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if !service {
		said, err := resetOnce()
		fmt.Println(said)
		return err
	}
	return svc.Run(resetServiceName, resetService{})
}

type resetService struct{}

// Execute does the reset once and stops: there is nothing to do until the next boot.
func (resetService) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	done := make(chan error, 1)
	go func() {
		_, err := resetOnce()
		done <- err
	}()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			status <- svc.Status{State: svc.StopPending}
			if err != nil {
				return true, 1
			}
			return false, 0
		case r := <-requests:
			switch r.Cmd {
			case svc.Interrogate:
				status <- r.CurrentStatus
			case svc.Stop, svc.Shutdown:
				// The boot entry changes only after a complete disk, so stopping in
				// the middle leaves the next boot as it would have been.
				status <- svc.Status{State: svc.StopPending}
				return false, 0
			}
		}
	}
}

// resetOnce finds the store, gives it a letter for the time of the reset, and
// writes what happened to the log on the store.
func resetOnce() (string, error) {
	volume, err := findStoreVolume()
	if err != nil {
		return "", err
	}
	letter, err := mountTemporary(volume)
	if err != nil {
		return "", err
	}
	defer windows.DeleteVolumeMountPoint(windows.StringToUTF16Ptr(letter + `:\`))
	ops := winResetOps{root: letter + `:\`}
	said, err := startup(ops)
	line := said
	if err != nil {
		line = "ошибка: " + err.Error()
	}
	ops.log(line)
	return said, err
}

// findStoreVolume finds the store by its label, without a letter: the partition
// attribute keeps Windows from giving it one.
func findStoreVolume() (string, error) {
	buf := make([]uint16, windows.MAX_PATH)
	h, err := windows.FindFirstVolume(&buf[0], uint32(len(buf)))
	if err != nil {
		return "", err
	}
	defer windows.FindVolumeClose(h)
	for {
		volume := windows.UTF16ToString(buf)
		label := make([]uint16, windows.MAX_PATH)
		if windows.GetVolumeInformation(windows.StringToUTF16Ptr(volume), &label[0], uint32(len(label)),
			nil, nil, nil, nil, 0) == nil && windows.UTF16ToString(label) == proto.StoreLabel {
			return volume, nil
		}
		if err := windows.FindNextVolume(h, &buf[0], uint32(len(buf))); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return "", fmt.Errorf("том %s не найден", proto.StoreLabel)
			}
			return "", err
		}
	}
}

// mountTemporary gives the volume the last free letter.
func mountTemporary(volume string) (string, error) {
	used, err := windows.GetLogicalDrives()
	if err != nil {
		return "", err
	}
	for i := 25; i >= 3; i-- { // Z: down to D:
		if used&(1<<i) != 0 {
			continue
		}
		letter := string(rune('A' + i))
		if err := windows.SetVolumeMountPoint(windows.StringToUTF16Ptr(letter+`:\`), windows.StringToUTF16Ptr(volume)); err != nil {
			return "", fmt.Errorf("буква для %s: %w", proto.StoreLabel, err)
		}
		return letter, nil
	}
	return "", errors.New("нет свободной буквы для хранилища")
}

// winResetOps works on the store mounted at root.
type winResetOps struct{ root string }

func (o winResetOps) path(name string) string { return filepath.Join(o.root, name) }

func (o winResetOps) ReadState() (proto.StoreState, error) {
	var st proto.StoreState
	b, err := os.ReadFile(o.path(proto.StateFile))
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

// WriteState replaces the file through a rename, so a power cut leaves the old
// state or the new one and never half of it.
func (o winResetOps) WriteState(st proto.StoreState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := o.path(proto.StateFile + ".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, o.path(proto.StateFile))
}

func (o winResetOps) Remove(name string) error {
	err := os.Remove(o.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if errors.Is(err, syscall.Errno(windows.ERROR_SHARING_VIOLATION)) {
		return errInUse
	}
	return err
}

func (o winResetOps) CreateDiff(name, parent string) error {
	return createVirtualDisk(o.path(name), o.path(parent), 0, 0)
}

// Point changes the entry Windows booted from. The letter only names the
// partition: the boot configuration keeps the partition, not the letter.
func (o winResetOps) Point(name string) error {
	vhd := fmt.Sprintf("vhd=[%s]\\%s", strings.TrimSuffix(o.root, `\`), name)
	for _, element := range []string{"device", "osdevice"} {
		out, err := exec.Command("bcdedit", "/set", "{current}", element, vhd).CombinedOutput()
		if err != nil {
			return fmt.Errorf("bcdedit %s: %w: %s", element, err, lastLine(out))
		}
	}
	return nil
}

func (o winResetOps) ReadAccounts() (*proto.Accounts, error) {
	b, err := os.ReadFile(o.path(proto.AccountsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a proto.Accounts
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (o winResetOps) RemoveAccounts() error { return os.Remove(o.path(proto.AccountsFile)) }

func (o winResetOps) ApplyAccounts(a *proto.Accounts) error { return applyAccounts(a) }

// Reboot restarts the PC a few seconds later, so the service has time to log
// and stop.
func (o winResetOps) Reboot() error {
	out, err := exec.Command("shutdown", "/r", "/t", "5", "/c", "locless-vhd: учётные записи настроены").CombinedOutput()
	if err != nil {
		return fmt.Errorf("shutdown: %w: %s", err, lastLine(out))
	}
	return nil
}

// log appends a line to the log on the store; a failure only loses the line.
func (o winResetOps) log(line string) {
	path := o.path(proto.ResetLog)
	if info, err := os.Stat(path); err == nil && info.Size() > resetLogLimit {
		if b, err := os.ReadFile(path); err == nil {
			os.WriteFile(path, b[len(b)/2:], 0o644)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\r\n", time.Now().Format("2006-01-02 15:04:05"), line)
}
