package proto

import (
	"fmt"
	"path"
	"strings"
)

// keep.vhdx is a disk of its own on the store that survives the reset: the
// caches that are slow to build again, shaders first of all. It is no part of
// the chain, so Windows writes into it directly. The reset service attaches it
// at every boot, mounts it at KeepMount and replaces the folders of the image
// setting with junctions into it. Players may write there but not run anything
// from it, so it cannot carry a cheat or a virus past the reset.
const (
	KeepFile       = "keep.vhdx"
	KeepConfigFile = "keep.json"
	KeepLabel      = "LVHD-KEEP"
	KeepMount      = `C:\LVHD\Keep`
	// PlayerProfile in a folder of the list stands for the profile of the player
	// user, wherever Windows put it.
	PlayerProfile = "%PLAYERPROFILE%"
)

// Limits of the setting.
const (
	DefaultKeepSize uint64 = 20 << 30
	MaxKeepSize     uint64 = 512 << 30
	MinKeepSize     uint64 = 1 << 30
	MaxKeepFolders         = 64
	maxKeepFolder          = 240
)

// Keep is the setting of an image: how large keep.vhdx is and what goes into
// it. Size 0 turns it off.
type Keep struct {
	Size    uint64   `json:"size"`
	Folders []string `json:"folders"`
}

// DefaultKeepFolders are the shader caches of the drivers and of DirectX.
var DefaultKeepFolders = []string{
	PlayerProfile + `\AppData\Local\NVIDIA\DXCache`,
	PlayerProfile + `\AppData\Local\NVIDIA\GLCache`,
	PlayerProfile + `\AppData\LocalLow\NVIDIA\PerDriverVersion\DXCache`,
	PlayerProfile + `\AppData\Local\AMD\DxCache`,
	PlayerProfile + `\AppData\Local\AMD\DxcCache`,
	PlayerProfile + `\AppData\Local\AMD\VkCache`,
	PlayerProfile + `\AppData\Local\D3DSCache`,
	`C:\ProgramData\NVIDIA Corporation\NV_Cache`,
}

// keepForbidden are folders whose replacement would break Windows or programs.
// A folder of the list may not be one of them or lie inside one, except where
// keepAllowedIn says otherwise.
var keepForbidden = []string{
	`c:\windows`, `c:\program files`, `c:\program files (x86)`, `c:\lvhd`,
	`c:\programdata\microsoft`, `c:\users\default`, `c:\users\public`,
}

// CheckKeepFolder says why a folder cannot be on the list, or "" if it can. A
// folder is absolute on C:, may start with PlayerProfile, and must lie at least
// three levels deep, so a whole profile or C:\Users is never replaced.
func CheckKeepFolder(folder string) string {
	f := strings.TrimSpace(folder)
	if f == "" || len(f) > maxKeepFolder {
		return "пустой путь или длиннее 240 символов"
	}
	if strings.ContainsAny(f, `/"*?<>|`) {
		return `путь пишется через \ и без "*?<>|`
	}
	rest, player := strings.CutPrefix(f, PlayerProfile)
	if player {
		f = `C:\Users\player` + rest
	} else if strings.Contains(f, "%") {
		return "из переменных есть только " + PlayerProfile
	}
	if len(f) < 3 || !strings.EqualFold(f[:3], `C:\`) {
		return `путь начинается с C:\ или ` + PlayerProfile
	}
	parts := strings.Split(f[3:], `\`)
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.HasSuffix(p, ".") || strings.HasSuffix(p, " ") {
			return "в пути пустая часть, . или .."
		}
	}
	// C:\Games\Steam at least; in a profile Users\<player>\AppData\Local\<vendor>,
	// so AppData or Local as a whole is never replaced.
	need := 2
	if player {
		need = 5
	}
	if len(parts) < need {
		return "слишком высоко: такую папку целиком заменять нельзя"
	}
	low := strings.ToLower(f)
	for _, no := range keepForbidden {
		if low == no || strings.HasPrefix(low, no+`\`) {
			return "внутри " + no + " папки менять нельзя"
		}
	}
	return ""
}

// KeepPlace is where a folder of the list lives in keep.vhdx: its path below the
// mount, the drive letter as the first part, so different folders never meet.
func KeepPlace(folder string) string {
	f := strings.TrimSpace(folder)
	if rest, ok := strings.CutPrefix(f, PlayerProfile); ok {
		f = `C:\player` + rest
	}
	clean := strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(f[3:], `\`, "/")), "/")
	return KeepMount + `\` + strings.ToUpper(f[:1]) + `\` + strings.ReplaceAll(clean, "/", `\`)
}

// ExpandKeepFolder turns a folder of the list into a path on this PC. A folder
// of the player needs the profile; without one it is skipped.
func ExpandKeepFolder(folder, profile string) (string, bool) {
	f := strings.TrimSpace(folder)
	rest, ok := strings.CutPrefix(f, PlayerProfile)
	if !ok {
		return f, true
	}
	if profile == "" {
		return "", false
	}
	return strings.TrimRight(profile, `\`) + rest, true
}

// Check validates the whole setting.
func (k Keep) Check() error {
	if k.Size != 0 && (k.Size < MinKeepSize || k.Size > MaxKeepSize) {
		return fmt.Errorf("размер keep: 0 (выключен) или от %d до %d ГБ", MinKeepSize>>30, MaxKeepSize>>30)
	}
	if len(k.Folders) > MaxKeepFolders {
		return fmt.Errorf("папок больше %d", MaxKeepFolders)
	}
	seen := map[string]bool{}
	for _, f := range k.Folders {
		if why := CheckKeepFolder(f); why != "" {
			return fmt.Errorf("%s: %s", f, why)
		}
		key := strings.ToLower(strings.TrimSpace(f))
		if seen[key] {
			return fmt.Errorf("%s: повторяется", f)
		}
		seen[key] = true
	}
	for a := range seen {
		for b := range seen {
			if a != b && strings.HasPrefix(b, a+`\`) {
				return fmt.Errorf("%s лежит внутри %s", b, a)
			}
		}
	}
	return nil
}
