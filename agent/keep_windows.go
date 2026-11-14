package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"locless-vhd/proto"
)

// keepLetter is where WinPE mounts keep.vhdx for the time it sets its rights.
const keepLetter = "K"

// keepSDDL lets players write keep.vhdx but not run anything from it. The deny
// ACE holds only for files (OIIO): FILE_EXECUTE on a folder is the right to pass
// through it. SYSTEM and administrators have full control.
const keepSDDL = "O:SYD:PAI(D;OIIO;0x20;;;BU)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1301bf;;;BU)"

// ensureKeep puts keep.vhdx and keep.json on the store as k says. Nil leaves both
// as they are; size 0 removes them. A keep.vhdx of the right size stays with
// what it holds: the caches are the point.
func (d *vhdDisk) ensureKeep(k *proto.Keep) error {
	if k == nil {
		return nil
	}
	if k.Size == 0 {
		for _, name := range []string{proto.KeepConfigFile, proto.KeepFile} {
			if err := os.Remove(storePath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return nil
	}
	if err := k.Check(); err != nil {
		return err
	}
	if err := d.makeKeep(k.Size); err != nil {
		return err
	}
	b, err := json.Marshal(k)
	if err != nil {
		return err
	}
	return os.WriteFile(storePath(proto.KeepConfigFile), b, 0o644)
}

// makeKeep makes keep.vhdx of size bytes, with one NTFS volume, unless it is
// there already.
func (d *vhdDisk) makeKeep(size uint64) error {
	path := storePath(proto.KeepFile)
	if f, err := os.Open(path); err == nil {
		l, lerr := proto.ReadVHDXLayout(f)
		f.Close()
		if lerr == nil && l.VirtualSize == size && !l.HasParent {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("старый %s: %w", proto.KeepFile, err)
		}
	}
	if err := createVirtualDisk(path, "", size, 512); err != nil {
		return err
	}
	if err := d.attach(proto.KeepFile); err != nil {
		return err
	}
	defer d.Close()
	// The partition gets no letter in Windows: the service mounts it in a folder.
	if err := runDiskpart(fmt.Sprintf("select disk %d", d.attached.number), "online disk noerr",
		"attributes disk clear readonly noerr", "convert gpt", "create partition primary",
		"format fs=ntfs quick label="+proto.KeepLabel, "assign letter="+keepLetter,
		"gpt attributes=0x8000000000000000"); err != nil {
		return err
	}
	defer windows.DeleteVolumeMountPoint(windows.StringToUTF16Ptr(keepLetter + `:\`))
	return protectWith(keepLetter+`:\`, keepSDDL)
}

// keepDisk stays attached for the life of the service: the volume goes when the
// handle closes.
var keepDisk *attachedDisk

// setupKeep attaches keep.vhdx, mounts it at proto.KeepMount and puts the
// junctions in place. It runs at the start of Windows, before a player is likely
// to have signed in, and says what it did for the log.
func setupKeep(ops winResetOps) string {
	b, err := os.ReadFile(ops.path(proto.KeepConfigFile))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		return "keep: " + err.Error()
	}
	var k proto.Keep
	if err := json.Unmarshal(b, &k); err != nil || k.Size == 0 {
		return ""
	}
	if keepDisk == nil {
		disk, err := attachVirtualDisk(ops.path(proto.KeepFile))
		if err != nil {
			return "keep: подключение: " + err.Error()
		}
		keepDisk = disk
	}
	volume, err := waitVolume(proto.KeepLabel, 10*time.Second)
	if err != nil {
		return "keep: " + err.Error()
	}
	if err := os.MkdirAll(proto.KeepMount, 0o755); err != nil {
		return "keep: " + err.Error()
	}
	if err := windows.SetVolumeMountPoint(windows.StringToUTF16Ptr(proto.KeepMount+`\`), windows.StringToUTF16Ptr(volume)); err != nil &&
		!errors.Is(err, windows.ERROR_DIR_NOT_EMPTY) {
		return "keep: точка монтирования: " + err.Error()
	}

	var player string
	if a, err := ops.ReadAccounts(); err == nil && a != nil {
		player = a.PlayerUser
	}
	links, skipped := keepLinks(k.Folders, playerProfile(player))
	var failed []string
	for _, l := range links {
		if err := placeLink(l); err != nil {
			failed = append(failed, l.folder+": "+err.Error())
		}
	}
	said := fmt.Sprintf("keep: папок %d", len(links)-len(failed))
	if len(skipped) > 0 {
		said += fmt.Sprintf(", пропущено %d (нет профиля игрока или папка недопустима)", len(skipped))
	}
	if len(failed) > 0 {
		said += ", ошибки: " + strings.Join(failed, "; ")
	}
	return said
}

// waitVolume waits for the volume with the label to appear after an attach.
func waitVolume(label string, wait time.Duration) (string, error) {
	end := time.Now().Add(wait)
	for {
		volume, err := findVolume(label)
		if err == nil || time.Now().After(end) {
			return volume, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// placeLink replaces a folder of the image with a junction into keep.vhdx. The
// folder lives in the differencing disk, which the reset makes fresh, so what it
// holds is the image's and goes; a junction there already was made earlier in
// this boot and stays.
func placeLink(l keepLink) error {
	if err := os.MkdirAll(l.place, 0o755); err != nil {
		return err
	}
	if attrs, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(l.folder)); err == nil {
		if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return nil
		}
		// A plain folder: RemoveAll stays inside it, there is no link to follow.
		if err := os.RemoveAll(l.folder); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(l.folder), 0o755); err != nil {
		return err
	}
	out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", l.folder, l.place).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mklink: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// playerProfile finds the profile folder of the player user, "" if the user or
// its profile does not exist yet. A folder made before Windows makes the profile
// would make Windows put the profile elsewhere, so then nothing is linked.
func playerProfile(name string) string {
	if name == "" {
		return ""
	}
	sid, _, _, err := windows.LookupSID("", name)
	if err != nil {
		return ""
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList\`+sid.String(), registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	path, _, err := k.GetStringValue("ProfileImagePath")
	if err != nil {
		return ""
	}
	if expanded, err := registry.ExpandString(path); err == nil {
		path = expanded
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return ""
	}
	return path
}
