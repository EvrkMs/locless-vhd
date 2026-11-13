package proto

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// TaskKindPrepare wipes the disk and lays it out for Windows booted from a
// virtual disk: EFI, MSR and one hidden NTFS store for the virtual disk files.
// It needs no image.
const TaskKindPrepare = "prepare"

// TaskKindDeployVHD writes an image into a new virtual disk on the store, lays
// the store out first if the disk has none, and makes Windows boot from a
// differencing disk on top of it.
const TaskKindDeployVHD = "deployvhd"

// The layout of a prepared disk.
const (
	// StoreLabel is the label of the NTFS volume that holds the virtual disks.
	StoreLabel = "LVHD"
	// StoreName is the GPT name of that partition, how a prepared disk is told
	// from any other disk with the same three kinds of partitions.
	StoreName = "locless-vhd store"
	// ESPSize fits FAT32 on a disk with 4 KiB sectors too.
	ESPSize = 260 << 20
	MSRSize = 16 << 20
	// MinStoreSize refuses disks too small to hold even one virtual disk.
	MinStoreSize = 16 << 30

	TypeESP       = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"
	TypeMSR       = "e3c9e316-0b5c-4db8-817d-f92df00215ae"
	TypeBasicData = "ebd0a0a2-b9e5-4433-87c0-68b6b72699c7"

	// AttrNoDriveLetter keeps Windows from giving the store a letter, so the
	// Windows booted from it does not show its own files to the players.
	AttrNoDriveLetter uint64 = 1 << 63

	storeAlign = 1 << 20
)

// ErrDiskTooSmall reports a disk that cannot hold the store.
var ErrDiskTooSmall = errors.New("proto: disk too small for the store")

// StoreLayout lays out the usable area of an empty GPT, from first to end in
// bytes: EFI, MSR and the store on all the rest. Partitions start on whole
// mebibytes, so blocks of a virtual disk on the store stay aligned.
func StoreLayout(first, end uint64, sector uint32) ([]Partition, error) {
	if sector < 512 || sector&(sector-1) != 0 {
		return nil, fmt.Errorf("proto: bad sector size %d", sector)
	}
	if end <= first {
		return nil, ErrDiskTooSmall
	}
	start := alignUp(first, storeAlign)
	storeStart := start + ESPSize + MSRSize
	storeEnd := end / storeAlign * storeAlign
	if storeEnd < storeStart || storeEnd-storeStart < MinStoreSize {
		return nil, fmt.Errorf("%w: %d bytes usable", ErrDiskTooSmall, end-first)
	}
	return []Partition{
		{Number: 1, TypeGUID: TypeESP, GUID: NewGUID(), Name: "EFI system partition",
			Offset: start, Size: ESPSize, Policy: PolicyFixed, Content: ContentNone},
		{Number: 2, TypeGUID: TypeMSR, GUID: NewGUID(), Name: "Microsoft reserved partition",
			Offset: start + ESPSize, Size: MSRSize, Policy: PolicyFixed, Content: ContentNone},
		{Number: 3, TypeGUID: TypeBasicData, GUID: NewGUID(), Name: StoreName, Attributes: AttrNoDriveLetter,
			Offset: storeStart, Size: storeEnd - storeStart, Policy: PolicyFill, Content: ContentNone},
	}, nil
}

// IsStoreLayout reports whether a table, in disk order, is the one StoreLayout
// makes. The file systems on it are not checked.
func IsStoreLayout(parts []Partition) bool {
	if len(parts) != 3 {
		return false
	}
	return equalGUID(parts[0].TypeGUID, TypeESP) && equalGUID(parts[1].TypeGUID, TypeMSR) &&
		equalGUID(parts[2].TypeGUID, TypeBasicData) && parts[2].Name == StoreName
}

// NewGUID returns a random GUID in the form 00000000-0000-4000-8000-000000000000.
func NewGUID() string {
	id := NewID()
	id[6] = id[6]&0x0f | 0x40 // version 4
	id[8] = id[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16])
}

func equalGUID(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if lower(a[i]) != lower(b[i]) {
			return false
		}
	}
	return true
}

func lower(c byte) byte {
	if 'A' <= c && c <= 'F' {
		return c + 'a' - 'A'
	}
	return c
}

func alignUp(n, to uint64) uint64 {
	return (n + to - 1) / to * to
}

// Files on the store.
const (
	BaseFile = "base.vhdx" // the image, block for block as on the server
	// The differencing disks end in .vhdx, not .avhdx as Hyper-V names them: the
	// boot loader tells the format by the extension and reads anything that is not
	// .vhdx as an old VHD, which fails with 0xc03a0001 (no VHD footer).
	LocalFile = "local.vhdx"  // on top of the base: what belongs to this PC only
	DiffFile  = "diff-a.vhdx" // on top of local: the differencing disk Windows boots from
	DiffFileB = "diff-b.vhdx" // its pair: the reset prepares one while Windows runs on the other
	StateFile = "locless-vhd.json"
	ResetLog  = "reset.log" // what the reset service did, on the store so a reset keeps it
	// AccountsFile holds the accounts the reset service sets on every boot. It
	// holds a password: the store lets only SYSTEM and administrators read it.
	AccountsFile = "accounts.json"
)

// Accounts is what the reset service does to the users of Windows on every boot.
// An empty field leaves that part alone.
type Accounts struct {
	// AdminPassword is set on the built-in administrator, which is enabled: UAC
	// asks a standard user for its name and this password.
	AdminPassword string `json:"adminPassword,omitempty"`
	// PlayerUser loses administrator rights; it is created if missing, without a
	// password. An account the image already has keeps its profile.
	PlayerUser string `json:"playerUser,omitempty"`
}

// Empty reports whether there is nothing to do.
func (a *Accounts) Empty() bool {
	return a == nil || a.AdminPassword == "" && a.PlayerUser == ""
}

// ValidUserName reports whether Windows accepts name for a local account.
func ValidUserName(name string) bool {
	if name == "" || len([]rune(name)) > 20 || strings.TrimRight(name, ". ") != name {
		return false
	}
	return !strings.ContainsAny(name, `"/\[]:;|=,+*?<>@`)
}

// OldFiles are names of the files above in earlier deploys, which a deploy
// removes along with the current ones.
var OldFiles = []string{"local.avhdx", "diff-a.avhdx", "diff-b.avhdx"}

// OtherDiff returns the differencing disk paired with name.
func OtherDiff(name string) string {
	if name == DiffFileB {
		return DiffFile
	}
	return DiffFileB
}

// StoreSpare is room on the store beyond the memory of the PC, for the files
// Windows keeps there when it boots from a virtual disk.
const StoreSpare = 4 << 30

// LocalSpare is room for local.vhdx. Only the agent writes into it, a little:
// the stretched volume and the name and service of the PC.
const LocalSpare = 1 << 30

// StoreNeed returns the size of store a deploy into VHDX needs.
//
// The deploy turns off the growing of the virtual disks to full size at boot
// (VirtualDiskExpandOnMount), so the files take what is written into them. Every
// write of Windows lands either in free space of the virtual disk, which the base
// file does not hold, or over data the base already holds: the second kind is
// the only one the store pays for twice, and overwriteMargin covers it. The page
// file and memory dumps live on the store too, so the memory of the PC is added.
func StoreNeed(vhdSize, memory uint64) uint64 {
	return vhdSize + overwriteMargin(vhdSize) + memory + StoreSpare + LocalSpare
}

// overwriteMargin is room for a session that rewrites data the base holds:
// a tenth of the virtual disk, at least 8 GiB.
func overwriteMargin(vhdSize uint64) uint64 {
	return max(vhdSize/10, 8<<30)
}

// AutoVHDSize is the largest virtual disk, in whole GiB, that StoreNeed lets a
// store of this size hold, or 0 if none fits.
func AutoVHDSize(store, memory uint64) uint64 {
	fixed := memory + StoreSpare + LocalSpare
	if store <= fixed {
		return 0
	}
	room := store - fixed
	size := min(room-min(room, 8<<30), room/11*10) // past 80 GiB the tenth is larger
	size = size >> 30 << 30
	for size > 0 && StoreNeed(size, memory) > store {
		size -= 1 << 30
	}
	return size
}

// StoreState is the state file on the store: what the base holds, so the server
// can tell which version a PC has.
type StoreState struct {
	Image   string    `json:"image"`
	Version int       `json:"version"`
	Format  string    `json:"format"` // "vhdx"
	VHDSize uint64    `json:"vhdSize"`
	Base    string    `json:"base"`
	Local   string    `json:"local,omitempty"` // the parent of the differencing disks; "" means the base
	Diff    string    `json:"diff"`
	Written time.Time `json:"written"`
}
