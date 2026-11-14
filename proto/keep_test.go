package proto

import (
	"strings"
	"testing"
)

func TestDefaultKeepIsValid(t *testing.T) {
	if err := (Keep{Size: DefaultKeepSize, Folders: DefaultKeepFolders}).Check(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckKeepFolder(t *testing.T) {
	good := []string{
		PlayerProfile + `\AppData\Local\NVIDIA\DXCache`,
		`C:\ProgramData\NVIDIA Corporation\NV_Cache`,
		`C:\Games\Steam\steamapps\shadercache`,
		`c:\games\cache`,
	}
	for _, f := range good {
		if why := CheckKeepFolder(f); why != "" {
			t.Errorf("%s refused: %s", f, why)
		}
	}
	bad := []string{
		``, `C:\`, `C:\Games`, `D:\Games\cache`, `relative\path`,
		`C:\Windows\System32\spool`, `C:\windows`, `C:\Program Files\Steam\cache`,
		`C:\Program Files (x86)\Steam\steamapps\shadercache`, `C:\LVHD\Keep\x`,
		`C:\Users\Default\AppData\x`, `C:\ProgramData\Microsoft\Windows`,
		PlayerProfile, PlayerProfile + `\AppData`, PlayerProfile + `\AppData\Local`,
		`%APPDATA%\NVIDIA\cache`, `C:\Games\..\Windows\x`, `C:\Games\a/b`, `C:\Games\*`,
		`C:\Games\x.`, `C:\Games\\x`,
	}
	for _, f := range bad {
		if why := CheckKeepFolder(f); why == "" {
			t.Errorf("%q accepted", f)
		}
	}
}

func TestKeepCheck(t *testing.T) {
	cases := map[string]Keep{
		"too small":  {Size: 1 << 20},
		"too large":  {Size: 1 << 40},
		"repeated":   {Size: DefaultKeepSize, Folders: []string{`C:\Games\cache`, `c:\games\CACHE`}},
		"nested":     {Size: DefaultKeepSize, Folders: []string{`C:\Games\cache`, `C:\Games\cache\sub`}},
		"bad folder": {Size: DefaultKeepSize, Folders: []string{`C:\Windows\x\y`}},
	}
	for name, k := range cases {
		if err := k.Check(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := (Keep{}).Check(); err != nil {
		t.Errorf("off: %v", err)
	}
}

func TestKeepPlaceAndExpand(t *testing.T) {
	if got := KeepPlace(PlayerProfile + `\AppData\Local\NVIDIA\DXCache`); got != KeepMount+`\C\player\AppData\Local\NVIDIA\DXCache` {
		t.Errorf("player: %s", got)
	}
	if got := KeepPlace(`c:\ProgramData\NVIDIA Corporation\NV_Cache`); got != KeepMount+`\C\ProgramData\NVIDIA Corporation\NV_Cache` {
		t.Errorf("program data: %s", got)
	}
	if got, ok := ExpandKeepFolder(PlayerProfile+`\AppData\Local\D3DSCache`, `C:\Users\Player.PC05\`); !ok || got != `C:\Users\Player.PC05\AppData\Local\D3DSCache` {
		t.Errorf("expand: %s %v", got, ok)
	}
	if _, ok := ExpandKeepFolder(PlayerProfile+`\AppData\Local\D3DSCache`, ""); ok {
		t.Error("expanded without a profile")
	}
	if got, ok := ExpandKeepFolder(` C:\Games\cache `, ""); !ok || strings.TrimSpace(got) != got {
		t.Errorf("plain: %q %v", got, ok)
	}
}
