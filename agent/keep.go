package main

import (
	"locless-vhd/proto"
)

// playerSetup is what the tasks that make the chain of the players again write
// beside it on the store: the accounts the reset service sets on every boot and
// keep.vhdx. A nil field leaves that part as the store has it.
type playerSetup struct {
	Accounts *proto.Accounts
	Keep     *proto.Keep
}

func setupOf(task *proto.AgentTask) playerSetup {
	return playerSetup{Accounts: task.Accounts, Keep: task.Keep}
}

// keepSize is the room keep.vhdx takes on the store, 0 without it.
func keepSize(k *proto.Keep) uint64 {
	if k == nil {
		return 0
	}
	return k.Size
}

// keepLink is one folder of the image replaced with a junction into keep.vhdx.
type keepLink struct {
	folder string // on C:, as the player sees it
	place  string // in keep.vhdx, below proto.KeepMount
}

// keepLinks plans the junctions for the folders of the setting. A folder of the
// player is skipped without a profile, and so is one the setting would refuse:
// keep.json lies on the store, and the service trusts nothing it reads there.
func keepLinks(folders []string, profile string) (links []keepLink, skipped []string) {
	for _, f := range folders {
		if proto.CheckKeepFolder(f) != "" {
			skipped = append(skipped, f)
			continue
		}
		path, ok := proto.ExpandKeepFolder(f, profile)
		if !ok {
			skipped = append(skipped, f)
			continue
		}
		links = append(links, keepLink{folder: path, place: proto.KeepPlace(f)})
	}
	return links, skipped
}
