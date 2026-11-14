package proto

import (
	"fmt"
)

// PageFile is the page file setting of an image. Windows reads it from
// PagingFiles in Memory Management at the very start of boot, before any
// service runs, so the agent writes it into local from WinPE: a service writing
// it would act only on the next boot, which starts from a fresh differencing
// disk without it. Save Mode leaves it alone, as save.vhdx goes into the image.
type PageFile struct {
	Mode  string `json:"mode"`  // one of the PageFile* modes
	MinMB uint32 `json:"minMb"` // for PageFileFixed
	MaxMB uint32 `json:"maxMb"`
}

// Page file modes.
const (
	PageFileImage = "image" // as the image has it: nothing is written
	PageFileAuto  = "auto"  // sized by Windows
	PageFileFixed = "fixed" // MinMB to MaxMB
	PageFileOff   = "off"   // none at all
)

// Limits of a fixed page file.
const (
	MinPageFileMB = 16
	MaxPageFileMB = 1 << 20 // 1 TiB
)

// DefaultPageFile changes nothing, as before the setting existed.
var DefaultPageFile = PageFile{Mode: PageFileImage}

// Check validates the setting.
func (p PageFile) Check() error {
	switch p.Mode {
	case PageFileImage, PageFileAuto, PageFileOff:
		return nil
	case PageFileFixed:
		if p.MinMB < MinPageFileMB || p.MaxMB > MaxPageFileMB || p.MinMB > p.MaxMB {
			return fmt.Errorf("размер файла подкачки: от %d до %d МБ, минимум не больше максимума", MinPageFileMB, MaxPageFileMB)
		}
		return nil
	}
	return fmt.Errorf("неизвестный режим файла подкачки %q", p.Mode)
}

// PagingFiles is the value of the registry for the setting, one line of a
// REG_MULTI_SZ, and whether there is anything to write. Off is an empty value.
func (p PageFile) PagingFiles() (string, bool) {
	switch p.Mode {
	case PageFileAuto:
		return `?:\pagefile.sys`, true
	case PageFileFixed:
		return fmt.Sprintf(`C:\pagefile.sys %d %d`, p.MinMB, p.MaxMB), true
	case PageFileOff:
		return "", true
	}
	return "", false
}

// Reserve is the room on the store the page file may take beyond what is kept
// for it anyway, the memory of the PC: only a fixed one larger than that.
func (p PageFile) Reserve(memory uint64) uint64 {
	if p.Mode != PageFileFixed {
		return 0
	}
	if max := uint64(p.MaxMB) << 20; max > memory {
		return max - memory
	}
	return 0
}
