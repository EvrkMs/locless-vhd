package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"locless-vhd/proto"
)

// Letters the deploy into VHDX gives volumes in WinPE, which has only X:.
const (
	storeLetter = "V" // the store on the physical disk
	espLetter   = "S" // the EFI partition of the physical disk, as in MakeBootable
)

// vhdDisk writes an image into base.vhdx on the store of the physical disk. Each
// step attaches the virtual disk it needs and detaches it when done, so Close
// leaves nothing attached and a retry finds the files free.
type vhdDisk struct {
	phys     *writeDisk
	size     uint64 // virtual size
	prepared bool   // the physical disk already has the store
	store    uint64 // size of the store partition, now or once laid out
	inner    *writeDisk
	attached *attachedDisk
}

// openVHDTarget picks the physical disk the same way as a deploy and plans the
// virtual disk on it. It changes nothing.
func openVHDTarget(size uint64) (vhdTarget, error) {
	number, err := chooseTarget(scanDisks())
	if err != nil {
		return nil, err
	}
	phys, err := newWriteDisk(number)
	if err != nil {
		return nil, err
	}
	d := &vhdDisk{phys: phys, size: size}
	if _, entries, err := parseLayout(phys.layout); err == nil && !phys.fresh {
		sort.Slice(entries, func(i, j int) bool { return entries[i].start < entries[j].start })
		parts := make([]proto.Partition, len(entries))
		for i, e := range entries {
			parts[i] = proto.Partition{TypeGUID: e.typeGUID, Name: e.name}
		}
		if proto.IsStoreLayout(parts) {
			d.prepared, d.store = true, entries[2].size
		}
	}
	if !d.prepared {
		first, end := gptArea(phys.size, phys.sector)
		parts, err := proto.StoreLayout(first, end, phys.sector)
		if err != nil {
			return nil, fmt.Errorf("диск не подходит под хранилище: %w", err)
		}
		d.store = parts[2].Size
	}
	return d, nil
}

func (d *vhdDisk) Describe() string {
	s := d.phys.Describe() + ", VHDX " + formatBytes(d.size)
	if !d.prepared {
		s += ", хранилище будет создано"
	}
	return s
}

// Fit plans the table inside the virtual disk and refuses a store too small for
// the worst case of proto.StoreNeed.
func (d *vhdDisk) Fit(m proto.Manifest) (proto.Manifest, error) {
	if need := proto.StoreNeed(d.size, physicalMemory()); d.store < need {
		return proto.Manifest{}, fmt.Errorf("хранилищу нужно %s (VHDX %s дважды, память ПК и запас), а на диске %s",
			formatBytes(need), formatBytes(d.size), formatBytes(d.store))
	}
	return fitFresh(m, d.size)
}

// Prepare lays the store out if there is none, replaces the files on it with an
// empty base.vhdx and writes the table of the image into it.
func (d *vhdDisk) Prepare(m proto.Manifest) error {
	if !d.prepared {
		if err := d.phys.MakeStore(); err != nil {
			return err
		}
		d.prepared = true
	}
	if err := mountStore(d.phys.number); err != nil {
		return err
	}
	for _, name := range []string{proto.StateFile, proto.DiffFile, proto.BaseFile} {
		if err := os.Remove(storePath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("старый файл %s: %w", name, err)
		}
	}
	if err := createVirtualDisk(storePath(proto.BaseFile), "", d.size, m.SectorSize); err != nil {
		return err
	}
	if err := d.attach(proto.BaseFile); err != nil {
		return err
	}
	inner, err := newWriteDisk(d.attached.number)
	if err != nil {
		return err
	}
	inner.allowFormat = true // it is our own file, not a disk of the PC
	d.inner = inner
	return inner.Prepare(m)
}

func (d *vhdDisk) WriteBlock(partition uint32, offset uint64, data []byte) error {
	if d.inner == nil {
		return errors.New("VHDX не подготовлен")
	}
	return d.inner.WriteBlock(partition, offset, data)
}

func (d *vhdDisk) Finish() error {
	if d.inner == nil {
		return errors.New("VHDX не подготовлен")
	}
	return d.inner.Finish()
}

// Close gives the partitions back and detaches the virtual disk.
func (d *vhdDisk) Close() error {
	var errs []error
	if d.inner != nil {
		errs = append(errs, d.inner.Close())
		d.inner = nil
	}
	errs = append(errs, d.attached.detach())
	d.attached = nil
	return errors.Join(errs...)
}

// Grow stretches the Windows volume inside the base over its partition.
func (d *vhdDisk) Grow(m proto.Manifest) error {
	if err := d.attach(proto.BaseFile); err != nil {
		return err
	}
	defer d.Close()
	return (&writeDisk{number: d.attached.number}).Grow(m)
}

// MakeBootable has nothing to do yet: the boot entry must point at the
// differencing disk, which Boot makes after the name is in the base.
func (d *vhdDisk) MakeBootable(proto.Manifest) error { return nil }

// Format and MakeStore belong to the tasks that wipe the physical disk.
func (d *vhdDisk) Format(string) error { return errors.New("не для VHDX") }
func (d *vhdDisk) MakeStore() error    { return errors.New("не для VHDX") }

// SetName writes the computer name into the base, so a reset of the differencing
// disk keeps it.
func (d *vhdDisk) SetName(name string) error {
	if !strings.EqualFold(os.Getenv("SystemDrive"), "X:") {
		return nil
	}
	if err := d.attach(proto.BaseFile); err != nil {
		return err
	}
	defer d.Close()
	if err := d.mountWindows(); err != nil {
		return err
	}
	return (&writeDisk{number: d.attached.number}).SetName(name)
}

// Boot puts the differencing disk on top of the base, adds the Windows on it to
// the boot menu of the firmware and writes the state file. Until it is done the
// PC has no Windows to boot, so unlike MakeBootable a failure fails the deploy.
func (d *vhdDisk) Boot(state proto.StoreState) error {
	if !strings.EqualFold(os.Getenv("SystemDrive"), "X:") {
		return errors.New("загрузочная запись VHDX работает только в WinPE")
	}
	if err := createVirtualDisk(storePath(proto.DiffFile), storePath(proto.BaseFile), 0, 0); err != nil {
		return err
	}
	if err := d.attach(proto.DiffFile); err != nil {
		return err
	}
	defer d.Close()
	if err := d.mountWindows(); err != nil {
		return err
	}
	if err := runDiskpart(fmt.Sprintf("select disk %d", d.phys.number), "rescan",
		"select partition 1", "assign letter="+espLetter+" noerr"); err != nil {
		return err
	}
	out, err := exec.Command("bcdboot", windowsLetter+`:\Windows`, "/s", espLetter+":", "/f", "UEFI").CombinedOutput()
	if err != nil {
		return fmt.Errorf("bcdboot: %w: %s", err, lastLine(out))
	}
	state.Base, state.Diff, state.Format = proto.BaseFile, proto.DiffFile, "vhdx"
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(storePath(proto.StateFile), b, 0o644)
}

// attach attaches a file of the store; one virtual disk at a time.
func (d *vhdDisk) attach(name string) error {
	if d.attached != nil {
		return errors.New("другой VHDX ещё подключён")
	}
	a, err := attachVirtualDisk(storePath(name))
	if err != nil {
		return err
	}
	d.attached = a
	return nil
}

// mountWindows gives the largest NTFS partition of the attached disk, the one the
// image fills, the letter SetName and bcdboot expect.
func (d *vhdDisk) mountWindows() error {
	layout, err := readTable(d.attached.number)
	if err != nil {
		return err
	}
	_, entries, err := parseLayout(layout)
	if err != nil {
		return err
	}
	var windows *tableEntry
	for i, e := range entries {
		if e.typeGUID == typeESP || e.typeGUID == typeMSR {
			continue
		}
		if windows == nil || e.size > windows.size {
			windows = &entries[i]
		}
	}
	if windows == nil {
		return errors.New("в VHDX нет раздела Windows")
	}
	return runDiskpart(fmt.Sprintf("select disk %d", d.attached.number), "rescan",
		fmt.Sprintf("select partition %d", windows.number), "assign letter="+windowsLetter+" noerr")
}

// mountStore gives the store its letter, which the partition attribute keeps
// Windows from doing.
func mountStore(disk int) error {
	return runDiskpart(fmt.Sprintf("select disk %d", disk), "rescan", "select partition 3",
		"remove all noerr", "assign letter="+storeLetter)
}

func storePath(name string) string {
	return filepath.Join(storeLetter+`:\`, name)
}
