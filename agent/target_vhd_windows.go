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

	"golang.org/x/sys/windows"

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
	if d.size == 0 { // the image leaves the size to the store
		if d.size = proto.AutoVHDSize(d.store, physicalMemory()); d.size == 0 {
			return nil, fmt.Errorf("хранилище %s слишком мало для VHDX", formatBytes(d.store))
		}
	}
	return d, nil
}

// VHDSize is the size the virtual disk gets, chosen by the store when the image
// leaves it open.
func (d *vhdDisk) VHDSize() uint64 { return d.size }

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
		return proto.Manifest{}, fmt.Errorf("хранилищу нужно %s (VHDX %s, запас на перезапись, память ПК), а на диске %s",
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
	if err := protectStore(storeLetter + `:\`); err != nil {
		return fmt.Errorf("права на хранилище: %w", err)
	}
	names := append([]string{proto.StateFile, proto.AccountsFile, proto.DiffFile, proto.DiffFileB, proto.LocalFile}, proto.OldFiles...)
	for _, name := range append(names, proto.BaseFile) {
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

// Grow stretches the Windows volume over its partition. It writes into local, so
// the base stays block for block the image on the server, and an update can
// write into it only the blocks that differ between versions.
func (d *vhdDisk) Grow(m proto.Manifest) error {
	if err := d.ensureLocal(); err != nil {
		return err
	}
	if err := d.attach(proto.LocalFile); err != nil {
		return err
	}
	defer d.Close()
	return (&writeDisk{number: d.attached.number}).Grow(m)
}

// MakeBootable has nothing to do yet: the boot entry must point at the
// differencing disk, which Boot makes after the name is in local.
func (d *vhdDisk) MakeBootable(proto.Manifest) error { return nil }

// Format and MakeStore belong to the tasks that wipe the physical disk.
func (d *vhdDisk) Format(string) error { return errors.New("не для VHDX") }
func (d *vhdDisk) MakeStore() error    { return errors.New("не для VHDX") }

// SetName writes the computer name into local: a reset of the differencing disk
// keeps it, and the base stays the same on every PC.
func (d *vhdDisk) SetName(name string) error {
	if !strings.EqualFold(os.Getenv("SystemDrive"), "X:") {
		return nil
	}
	if err := d.ensureLocal(); err != nil {
		return err
	}
	if err := d.attach(proto.LocalFile); err != nil {
		return err
	}
	defer d.Close()
	if err := d.mountWindows(); err != nil {
		return err
	}
	return (&writeDisk{number: d.attached.number}).SetName(name)
}

// Boot puts the differencing disk on top of local, adds the Windows on it to
// the boot menu of the firmware and writes the state file. Until it is done the
// PC has no Windows to boot, so unlike MakeBootable a failure fails the deploy.
//
// With accounts to set up, the first boot goes straight to local instead: users
// and passwords are changed by Windows itself, and what it writes then must stay.
// The reset service does the accounts on that boot, then makes the differencing
// disk, points the entry at it and restarts the PC.
func (d *vhdDisk) Boot(state proto.StoreState, accounts *proto.Accounts) error {
	if !strings.EqualFold(os.Getenv("SystemDrive"), "X:") {
		return errors.New("загрузочная запись VHDX работает только в WinPE")
	}
	if err := d.ensureLocal(); err != nil {
		return err
	}
	if err := d.installReset(); err != nil {
		return fmt.Errorf("служба сброса: %w", err)
	}
	top := proto.DiffFile
	if accounts.Empty() {
		if err := createVirtualDisk(storePath(proto.DiffFile), storePath(proto.LocalFile), 0, 0); err != nil {
			return err
		}
	} else {
		b, err := json.Marshal(accounts)
		if err != nil {
			return err
		}
		if err := os.WriteFile(storePath(proto.AccountsFile), b, 0o600); err != nil {
			return err
		}
		top = proto.LocalFile
	}
	if err := d.attach(top); err != nil {
		return err
	}
	defer d.Close()
	if err := d.mountWindows(); err != nil {
		return err
	}
	// The EFI partition is formatted first. A deploy onto a disk that already has
	// the store keeps the partitions, and bcdboot adds to the boot configuration it
	// finds: entries of earlier deploys stayed in the boot menu, pointing at files
	// that are gone, and the PC could boot one of them.
	if err := runDiskpart(fmt.Sprintf("select disk %d", d.phys.number), "rescan",
		"select partition 1", "format fs=fat32 quick label=System", "assign letter="+espLetter+" noerr"); err != nil {
		return fmt.Errorf("раздел EFI: %w", err)
	}
	out, err := exec.Command("bcdboot", windowsLetter+`:\Windows`, "/s", espLetter+":", "/f", "UEFI").CombinedOutput()
	if err != nil {
		return fmt.Errorf("bcdboot: %w: %s", err, lastLine(out))
	}
	state.Base, state.Local, state.Format = proto.BaseFile, proto.LocalFile, "vhdx"
	if top == proto.DiffFile {
		state.Diff = proto.DiffFile
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(storePath(proto.StateFile), b, 0o644)
}

// installReset puts the agent into local as the reset service, so every
// differencing disk made on top of it has it.
func (d *vhdDisk) installReset() (err error) {
	if err := d.attach(proto.LocalFile); err != nil {
		return err
	}
	defer d.Close()
	if err := d.mountWindows(); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := copyFile(self, windowsLetter+`:\Windows\System32\`+resetExe); err != nil {
		return err
	}
	const hive = `HKLM\LOCLESS`
	if err := runReg("load", hive, windowsLetter+`:\Windows\System32\config\SYSTEM`); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, runReg("unload", hive)) }()
	out, err := exec.Command("reg", "query", hive+`\Select`, "/v", "Current").CombinedOutput()
	if err != nil {
		return fmt.Errorf("reg query: %w: %s", err, lastLine(out))
	}
	set, err := controlSet(string(out))
	if err != nil {
		return err
	}
	key := hive + `\` + set + `\Services\` + resetServiceName
	for _, v := range [][3]string{
		{"Type", "REG_DWORD", "16"},        // a service in its own process
		{"Start", "REG_DWORD", "2"},        // automatic
		{"ErrorControl", "REG_DWORD", "1"}, // log and go on booting
		{"ImagePath", "REG_EXPAND_SZ", `%SystemRoot%\System32\` + resetExe + " " + resetFlag},
		{"ObjectName", "REG_SZ", "LocalSystem"},
		{"DisplayName", "REG_SZ", "locless-vhd: сброс разностного диска"},
	} {
		if err := runReg("add", key, "/v", v[0], "/t", v[1], "/d", v[2], "/f"); err != nil {
			return err
		}
	}
	// Windows booted from a dynamic virtual disk grows it to full size first, to
	// be sure the store holds it. With the disk as large as the store allows that
	// would not fit; StoreNeed counts what the files can really take instead.
	expand := hive + `\` + set + `\Services\FsDepends\Parameters`
	if err := runReg("add", expand, "/v", "VirtualDiskExpandOnMount", "/t", "REG_DWORD", "/d", "4", "/f"); err != nil {
		return err
	}
	return enableUAC()
}

// enableUAC turns UAC on in the Windows mounted at W:. A standard user who asks
// for more rights gets a prompt for the name and password of an administrator on
// the secure desktop; an administrator is asked to confirm, as Windows does by
// default. EnableLUA takes effect at boot, and this Windows has not booted yet.
func enableUAC() (err error) {
	const hive = `HKLM\LOCLESSSW`
	if err := runReg("load", hive, windowsLetter+`:\Windows\System32\config\SOFTWARE`); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, runReg("unload", hive)) }()
	key := hive + `\Microsoft\Windows\CurrentVersion\Policies\System`
	for _, v := range [][2]string{
		{"EnableLUA", "1"},
		{"ConsentPromptBehaviorUser", "1"},  // credentials, on the secure desktop
		{"ConsentPromptBehaviorAdmin", "5"}, // consent for programs not from Windows
		{"PromptOnSecureDesktop", "1"},
	} {
		if err := runReg("add", key, "/v", v[0], "/t", "REG_DWORD", "/d", v[1], "/f"); err != nil {
			return err
		}
	}
	return nil
}

// resetExe is the name of the agent inside the deployed Windows.
const resetExe = "lvhd-reset.exe"

func copyFile(from, to string) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, b, 0o755)
}

// ensureLocal makes local.vhdx on top of the base once per deploy: Prepare
// removed the one of the deploy before.
func (d *vhdDisk) ensureLocal() error {
	if _, err := os.Stat(storePath(proto.LocalFile)); err == nil {
		return nil
	}
	return createVirtualDisk(storePath(proto.LocalFile), storePath(proto.BaseFile), 0, 0)
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

// storeSDDL gives the store to SYSTEM: full control for it, which the boot
// loader, the agent in WinPE and the reset service all run as, and reading for
// administrators. Players have no business reading the image or the password
// waiting for the first boot, so other users get nothing. The DACL is protected, so nothing else is
// inherited, and files made later get the same through OICI.
const storeSDDL = "O:SYD:PAI(A;OICI;FA;;;SY)(A;OICI;FRFX;;;BA)"

// protectStore replaces the owner and the whole DACL of the store root. An owner
// may always rewrite the DACL, so the owner becomes SYSTEM too. Administrators can
// still take ownership back: this keeps the players out, not the operator.
func protectStore(root string) error {
	sd, err := windows.SecurityDescriptorFromString(storeSDDL)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner, nil, dacl, nil)
}
