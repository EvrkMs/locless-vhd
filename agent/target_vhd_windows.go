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
	keep     uint64 // room kept on the store beside the VHDX: keep.vhdx, a large page file
}

// openVHDTarget picks the physical disk the same way as a deploy and plans the
// virtual disk on it. It changes nothing.
func openVHDTarget(size, keep uint64) (vhdTarget, error) {
	number, err := chooseTarget(scanDisks())
	if err != nil {
		return nil, err
	}
	phys, err := newWriteDisk(number)
	if err != nil {
		return nil, err
	}
	d := &vhdDisk{phys: phys, size: size, keep: keep}
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
		if d.size = proto.AutoVHDSize(d.store, physicalMemory()+d.keep); d.size == 0 {
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
	if need := proto.StoreNeed(d.size, physicalMemory()+d.keep); d.store < need {
		return proto.Manifest{}, fmt.Errorf("хранилищу нужно %s (VHDX %s, запас на перезапись, память ПК, keep), а на диске %s",
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
	names := append([]string{proto.BaseIndexFile, proto.StateFile, proto.AccountsFile, proto.DiffFile, proto.DiffFileB,
		proto.SaveFile, proto.LocalFile}, proto.OldFiles...)
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

// Held reads what the last deploy wrote into base.vhdx, if it finished.
func (d *vhdDisk) Held(image string) (proto.BaseIndex, bool) {
	if !d.prepared || mountStore(d.phys.number) != nil {
		return proto.BaseIndex{}, false
	}
	if _, err := os.Stat(storePath(proto.BaseFile)); err != nil {
		return proto.BaseIndex{}, false
	}
	b, err := os.ReadFile(storePath(proto.BaseIndexFile))
	if err != nil {
		return proto.BaseIndex{}, false
	}
	x, err := proto.ParseBaseIndex(b)
	if err != nil || x.Image != image {
		return proto.BaseIndex{}, false
	}
	return x, true
}

// Reopen keeps base.vhdx and its table for an update. The record of what it
// holds goes first: from the first write on the base is neither version. Every
// disk on top of the base is made from it and stops being valid once it changes,
// so they all go, as in Prepare.
func (d *vhdDisk) Reopen(m proto.Manifest) error {
	if err := mountStore(d.phys.number); err != nil {
		return err
	}
	if err := protectStore(storeLetter + `:\`); err != nil {
		return fmt.Errorf("права на хранилище: %w", err)
	}
	names := append([]string{proto.BaseIndexFile, proto.StateFile, proto.AccountsFile, proto.DiffFile, proto.DiffFileB,
		proto.SaveFile, proto.LocalFile}, proto.OldFiles...)
	for _, name := range names {
		if err := os.Remove(storePath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("старый файл %s: %w", name, err)
		}
	}
	if err := d.attach(proto.BaseFile); err != nil {
		return err
	}
	inner, err := newWriteDisk(d.attached.number)
	if err != nil {
		return err
	}
	d.inner = inner
	return inner.Reopen(m)
}

// Remember records what base.vhdx holds now.
func (d *vhdDisk) Remember(x proto.BaseIndex) error {
	b, err := x.MarshalBinary()
	if err != nil {
		return err
	}
	path := storePath(proto.BaseIndexFile)
	if err := os.WriteFile(path+".new", b, 0o644); err != nil {
		return err
	}
	return os.Rename(path+".new", path)
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
// Accounts go into a file on the store that stays: users and passwords live in
// the SAM, which only a running Windows can change, so the reset service sets
// them on every boot. Booting local itself to set them once does not work: no
// differencing disk can be made on top of the disk Windows runs from.
func (d *vhdDisk) Boot(state proto.StoreState, setup playerSetup) error {
	accounts := setup.Accounts
	if !strings.EqualFold(os.Getenv("SystemDrive"), "X:") {
		return errors.New("загрузочная запись VHDX работает только в WinPE")
	}
	if err := d.ensureLocal(); err != nil {
		return err
	}
	if err := d.installReset(proto.LocalFile); err != nil {
		return fmt.Errorf("служба сброса: %w", err)
	}
	if err := d.setPageFile(setup.PageFile); err != nil {
		return fmt.Errorf("файл подкачки: %w", err)
	}
	if !accounts.Empty() {
		b, err := json.Marshal(accounts)
		if err != nil {
			return err
		}
		if err := os.WriteFile(storePath(proto.AccountsFile), b, 0o600); err != nil {
			return err
		}
	}
	if err := d.ensureKeep(setup.Keep); err != nil {
		return fmt.Errorf("keep: %w", err)
	}
	// What a session or Save Mode left is gone once the new entry is in place.
	for _, name := range []string{proto.DiffFile, proto.DiffFileB} {
		if err := os.Remove(storePath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("старый файл %s: %w", name, err)
		}
	}
	if err := createVirtualDisk(storePath(proto.DiffFile), storePath(proto.LocalFile), 0, 0); err != nil {
		return err
	}
	if err := d.bootFrom(proto.DiffFile); err != nil {
		return err
	}
	state.Base, state.Local, state.Diff, state.Format, state.Save = proto.BaseFile, proto.LocalFile, proto.DiffFile, "vhdx", ""
	return writeState(state)
}

// bootFrom makes the firmware boot the Windows of a file of the store.
func (d *vhdDisk) bootFrom(name string) error {
	if err := d.attach(name); err != nil {
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
	return nil
}

func writeState(state proto.StoreState) error {
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(storePath(proto.StateFile), b, 0o644)
}

func readState() (proto.StoreState, error) {
	var st proto.StoreState
	b, err := os.ReadFile(storePath(proto.StateFile))
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

// setPageFile writes the page file setting into local. Windows reads it at the
// very start of boot, so only an offline write lasts past the reset.
func (d *vhdDisk) setPageFile(p *proto.PageFile) (err error) {
	if p == nil {
		return nil
	}
	value, write := p.PagingFiles()
	if !write {
		return nil
	}
	if err := d.attach(proto.LocalFile); err != nil {
		return err
	}
	defer d.Close()
	if err := d.mountWindows(); err != nil {
		return err
	}
	const hive = `HKLM\LOCLESSPF`
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
	key := hive + `\` + set + `\Control\Session Manager\Memory Management`
	return runReg("add", key, "/v", "PagingFiles", "/t", "REG_MULTI_SZ", "/d", value, "/f")
}

// installReset puts the agent as the reset service into a file of the store:
// local, so every differencing disk made on top of it has it, or save.vhdx.
func (d *vhdDisk) installReset(name string) (err error) {
	if err := d.attach(name); err != nil {
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
	// Safe mode starts only the services listed under SafeBoot. Without the reset
	// there the session of a safe boot would be the start of the next one.
	for _, mode := range []string{"Minimal", "Network"} {
		safe := hive + `\` + set + `\Control\SafeBoot\` + mode + `\` + resetServiceName
		if err := runReg("add", safe, "/ve", "/t", "REG_SZ", "/d", "Service", "/f"); err != nil {
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
	number, err := d.windowsPartition()
	if err != nil {
		return err
	}
	return runDiskpart(fmt.Sprintf("select disk %d", d.attached.number), "rescan",
		fmt.Sprintf("select partition %d", number), "assign letter="+windowsLetter+" noerr")
}

// windowsPartition returns the number of the largest partition of the attached
// disk that is neither EFI nor MSR: the Windows volume.
func (d *vhdDisk) windowsPartition() (uint32, error) {
	layout, err := readTable(d.attached.number)
	if err != nil {
		return 0, err
	}
	_, entries, err := parseLayout(layout)
	if err != nil {
		return 0, err
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
		return 0, errors.New("в VHDX нет раздела Windows")
	}
	return windows.number, nil
}

// vhdStore is the store of a PC that holds a deploy into VHDX, for Save Mode.
type vhdStore struct {
	d     *vhdDisk
	state proto.StoreState
}

// openSavedStore finds the prepared store of this PC, gives it its letter and
// reads the state file.
func openSavedStore() (savedStore, error) {
	if !strings.EqualFold(os.Getenv("SystemDrive"), "X:") {
		return nil, errors.New("хранилище меняется только из WinPE")
	}
	number, err := chooseTarget(scanDisks())
	if err != nil {
		return nil, err
	}
	phys, err := newWriteDisk(number)
	if err != nil {
		return nil, err
	}
	_, entries, err := parseLayout(phys.layout)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].start < entries[j].start })
	parts := make([]proto.Partition, len(entries))
	for i, e := range entries {
		parts[i] = proto.Partition{TypeGUID: e.typeGUID, Name: e.name}
	}
	if phys.fresh || !proto.IsStoreLayout(parts) {
		return nil, errors.New("на диске нет хранилища LVHD")
	}
	if err := mountStore(number); err != nil {
		return nil, err
	}
	st, err := readState()
	if err != nil {
		return nil, fmt.Errorf("файл состояния: %w", err)
	}
	return &vhdStore{d: &vhdDisk{phys: phys, prepared: true}, state: st}, nil
}

func (s *vhdStore) State() proto.StoreState { return s.state }

func (s *vhdStore) Close() error {
	if s.d.attached != nil {
		return s.d.Close()
	}
	return nil
}

// EnterSave makes save.vhdx on top of the base. The base holds the volume as the
// clone shrank it and local stretches it; save.vhdx has no local below, so the
// volume is stretched in it. The reset service goes there too: in Save Mode it
// does nothing, but without it the virtual disk would grow to its full size at
// boot. The state file changes only after the boot entry points at save.vhdx, and
// the differencing disks of the players go last: until then the PC boots as
// before.
func (s *vhdStore) EnterSave() error {
	d := s.d
	if err := os.Remove(storePath(proto.SaveFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("старый %s: %w", proto.SaveFile, err)
	}
	if err := createVirtualDisk(storePath(proto.SaveFile), storePath(proto.BaseFile), 0, 0); err != nil {
		return err
	}
	if err := d.attach(proto.SaveFile); err != nil {
		return err
	}
	number, err := d.windowsPartition()
	if err == nil {
		err = runDiskpart(fmt.Sprintf("select disk %d", d.attached.number), "rescan",
			fmt.Sprintf("select partition %d", number), "extend filesystem")
	}
	if closeErr := d.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("том в %s не растянут: %w", proto.SaveFile, err)
	}
	if err := d.installReset(proto.SaveFile); err != nil {
		return fmt.Errorf("служба сброса: %w", err)
	}
	if err := d.bootFrom(proto.SaveFile); err != nil {
		return err
	}
	st := s.state
	st.Save, st.Diff = proto.SaveFile, ""
	if err := writeState(st); err != nil {
		return err
	}
	s.state = st
	for _, name := range []string{proto.DiffFile, proto.DiffFileB} {
		os.Remove(storePath(name)) // a session of the players, nothing more; space for save.vhdx
	}
	return nil
}

// LeaveSave boots the usual chain again and then removes save.vhdx.
func (s *vhdStore) LeaveSave(setup playerSetup) error {
	st := s.state
	st.Save = ""
	if err := s.d.Boot(st, setup); err != nil {
		return err
	}
	s.state, _ = readState()
	if err := os.Remove(storePath(proto.SaveFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s не удалён: %w", proto.SaveFile, err)
	}
	return nil
}

// OpenSaved attaches save.vhdx, and with it the base, as a disk to clone. Closing
// the source detaches it.
func (s *vhdStore) OpenSaved() (source, error) {
	if err := s.d.attach(proto.SaveFile); err != nil {
		return nil, err
	}
	src, err := openDisk(s.d.attached.number)
	if err != nil {
		s.d.Close()
		return nil, err
	}
	return &attachedSource{physicalDisk: src, detach: s.d.Close}, nil
}

// UpdateService writes this agent as the reset service. In Save Mode it goes
// into save.vhdx, the disk Windows boots from, with nothing on top. Otherwise it
// goes into local, under the differencing disks: Boot writes it there and makes a
// fresh differencing disk with the boot entry, as the old ones are made from the
// local before the change.
func (s *vhdStore) UpdateService(setup playerSetup) error {
	if s.state.Save != "" {
		return s.d.installReset(proto.SaveFile)
	}
	if err := s.d.Boot(s.state, setup); err != nil {
		return err
	}
	s.state, _ = readState()
	return nil
}

// Changes reads the BAT of save.vhdx. The file must not be attached: the BAT of an
// attached disk may still sit in its log.
func (s *vhdStore) Changes() (proto.VHDXLayout, error) {
	f, err := os.Open(storePath(proto.SaveFile))
	if err != nil {
		return proto.VHDXLayout{}, err
	}
	defer f.Close()
	l, err := proto.ReadVHDXLayout(f)
	if err != nil {
		return l, err
	}
	if !l.HasParent {
		return l, fmt.Errorf("%s не разностный диск", proto.SaveFile)
	}
	return l, nil
}

// attachedSource is a disk to clone that lives in an attached virtual disk.
type attachedSource struct {
	*physicalDisk
	detach func() error
}

func (a *attachedSource) Close() error {
	return errors.Join(a.physicalDisk.Close(), a.detach())
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
func protectStore(root string) error { return protectWith(root, storeSDDL) }

// protectWith replaces the owner and the whole DACL of root with those of sddl.
func protectWith(root, sddl string) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
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
