package main

import (
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// The virtual disk API of Windows, virtdisk.dll. Windows boots from such a disk
// itself, so the agent only makes the files and attaches them while it writes.
var (
	virtdisk                       = syscall.NewLazyDLL("virtdisk.dll")
	procCreateVirtualDisk          = virtdisk.NewProc("CreateVirtualDisk")
	procOpenVirtualDisk            = virtdisk.NewProc("OpenVirtualDisk")
	procAttachVirtualDisk          = virtdisk.NewProc("AttachVirtualDisk")
	procGetVirtualDiskPhysicalPath = virtdisk.NewProc("GetVirtualDiskPhysicalPath")
)

// Device types of VIRTUAL_STORAGE_TYPE.
const (
	storageTypeVHD  = 2
	storageTypeVHDX = 3
)

// vendorMicrosoft is VIRTUAL_STORAGE_TYPE_VENDOR_MICROSOFT, as Windows lays the GUID out.
var vendorMicrosoft = [16]byte{0xec, 0x4a, 0x98, 0xec, 0xf9, 0xa0, 0xe9, 0x47, 0x90, 0x1f, 0x71, 0x41, 0x5a, 0x66, 0x34, 0x5b}

const (
	attachNoDriveLetter = 0x2 // ATTACH_VIRTUAL_DISK_FLAG_NO_DRIVE_LETTER
	vhdBlockSize        = 1 << 20
	vhdPhysicalSector   = 4096
)

// storageType is VIRTUAL_STORAGE_TYPE.
type storageType struct {
	deviceID uint32
	vendorID [16]byte
}

// createParams2 is CREATE_VIRTUAL_DISK_PARAMETERS version 2. The union after the
// version holds a ULONGLONG, so it starts at offset 8; Go pads the same way.
type createParams2 struct {
	version            uint32
	_                  uint32
	uniqueID           [16]byte
	maximumSize        uint64
	blockSize          uint32
	sectorSize         uint32
	physicalSectorSize uint32
	_                  uint32
	parentPath         *uint16
	sourcePath         *uint16
	openFlags          uint32
	parentType         storageType
	sourceType         storageType
	resiliencyGUID     [16]byte
}

// openParams2 is OPEN_VIRTUAL_DISK_PARAMETERS version 2.
type openParams2 struct {
	version        uint32
	getInfoOnly    int32
	readOnly       int32
	resiliencyGUID [16]byte
}

// attachParams1 is ATTACH_VIRTUAL_DISK_PARAMETERS version 1.
type attachParams1 struct {
	version  uint32
	reserved uint32
}

// storageFor picks the format by the extension, as Windows itself does: .vhdx and
// .avhdx are VHDX, anything else VHD.
func storageFor(path string) storageType {
	t := storageType{deviceID: storageTypeVHD, vendorID: vendorMicrosoft}
	if lower := strings.ToLower(path); strings.HasSuffix(lower, ".vhdx") || strings.HasSuffix(lower, ".avhdx") {
		t.deviceID = storageTypeVHDX
	}
	return t
}

// createVirtualDisk makes a dynamic virtual disk of size bytes with blocks of
// 1 MiB, so each block is one block of the image. With parent set it makes a
// differencing disk on top of that file instead, and size and sector are taken
// from the parent.
func createVirtualDisk(path, parent string, size uint64, sector uint32) error {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	p := createParams2{version: 2, blockSize: vhdBlockSize}
	if parent == "" {
		p.maximumSize, p.sectorSize, p.physicalSectorSize = size, sector, vhdPhysicalSector
	} else if p.parentPath, err = syscall.UTF16PtrFromString(parent); err != nil {
		return err
	}
	t := storageFor(path)
	var h syscall.Handle
	// Version 2 wants VIRTUAL_DISK_ACCESS_NONE and no security descriptor.
	r, _, _ := procCreateVirtualDisk.Call(uintptr(unsafe.Pointer(&t)), uintptr(unsafe.Pointer(name)), 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&p)), 0, uintptr(unsafe.Pointer(&h)))
	if r != 0 {
		return fmt.Errorf("создание %s: %w", path, syscall.Errno(r))
	}
	return syscall.CloseHandle(h)
}

// attachedDisk is a virtual disk attached for as long as its handle is open:
// closing it, or the end of the agent, detaches it.
type attachedDisk struct {
	handle syscall.Handle
	number int // \\.\PhysicalDriveN
}

// attachVirtualDisk attaches a virtual disk, with its whole chain of parents,
// without giving its volumes letters, and finds its disk number.
func attachVirtualDisk(path string) (*attachedDisk, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	t := storageFor(path)
	op := openParams2{version: 2}
	var h syscall.Handle
	r, _, _ := procOpenVirtualDisk.Call(uintptr(unsafe.Pointer(&t)), uintptr(unsafe.Pointer(name)), 0, 0,
		uintptr(unsafe.Pointer(&op)), uintptr(unsafe.Pointer(&h)))
	if r != 0 {
		return nil, fmt.Errorf("открытие %s: %w", path, syscall.Errno(r))
	}
	ap := attachParams1{version: 1}
	r, _, _ = procAttachVirtualDisk.Call(uintptr(h), 0, attachNoDriveLetter, 0, uintptr(unsafe.Pointer(&ap)), 0)
	if r != 0 {
		syscall.CloseHandle(h)
		return nil, fmt.Errorf("подключение %s: %w", path, syscall.Errno(r))
	}
	buf := make([]uint16, 260)
	size := uint32(len(buf) * 2)
	r, _, _ = procGetVirtualDiskPhysicalPath.Call(uintptr(h), uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&buf[0])))
	if r != 0 {
		syscall.CloseHandle(h)
		return nil, fmt.Errorf("номер диска %s: %w", path, syscall.Errno(r))
	}
	number, err := driveNumber(syscall.UTF16ToString(buf))
	if err != nil {
		syscall.CloseHandle(h)
		return nil, err
	}
	return &attachedDisk{handle: h, number: number}, nil
}

// driveNumber reads N from \\.\PhysicalDriveN.
func driveNumber(path string) (int, error) {
	i := strings.LastIndex(strings.ToLower(path), "physicaldrive")
	if i < 0 {
		return 0, fmt.Errorf("неожиданный путь диска %q", path)
	}
	n, err := strconv.Atoi(path[i+len("physicaldrive"):])
	if err != nil {
		return 0, fmt.Errorf("неожиданный путь диска %q", path)
	}
	return n, nil
}

// detach closes the handle, which detaches the disk. It may be called again.
func (a *attachedDisk) detach() error {
	if a == nil || a.handle == 0 {
		return nil
	}
	err := syscall.CloseHandle(a.handle)
	a.handle = 0
	return err
}
