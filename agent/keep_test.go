package main

import (
	"testing"

	"locless-vhd/proto"
)

func TestKeepLinks(t *testing.T) {
	folders := []string{
		proto.PlayerProfile + `\AppData\Local\NVIDIA\DXCache`,
		`C:\ProgramData\NVIDIA Corporation\NV_Cache`,
		`C:\Windows\System32\drivers`, // keep.json was tampered with
	}
	links, skipped := keepLinks(folders, `C:\Users\Player`)
	if len(links) != 2 || len(skipped) != 1 || skipped[0] != folders[2] {
		t.Fatalf("links %+v, skipped %v", links, skipped)
	}
	if links[0].folder != `C:\Users\Player\AppData\Local\NVIDIA\DXCache` ||
		links[0].place != proto.KeepMount+`\C\player\AppData\Local\NVIDIA\DXCache` {
		t.Fatalf("player link %+v", links[0])
	}
	if links[1].folder != folders[1] || links[1].place != proto.KeepMount+`\C\ProgramData\NVIDIA Corporation\NV_Cache` {
		t.Fatalf("program data link %+v", links[1])
	}

	// No player profile yet: its folders wait, the others go on.
	links, skipped = keepLinks(folders[:2], "")
	if len(links) != 1 || len(skipped) != 1 {
		t.Fatalf("without a profile: links %+v, skipped %v", links, skipped)
	}
}
