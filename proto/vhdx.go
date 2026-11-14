package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// A read-only reader of the VHDX format, [MS-VHDX] version 1: just enough to list
// the blocks a differencing disk holds itself. Everything else of the disk comes
// from its parent. Save Mode sends only those blocks into the image.
//
//	0       file identifier "vhdxfile"
//	64 KiB  header 1, 128 KiB header 2: the one with the higher sequence number
//	192 KiB region table 1, 256 KiB region table 2: where the BAT and metadata are
//	BAT     one 64-bit entry per payload block, with a sector bitmap entry after
//	        every chunk ratio of them
//
// Integers are little-endian; GUIDs are stored as Windows lays them out.

// VHDXLayout is what a VHDX file says about itself.
type VHDXLayout struct {
	BlockSize   uint32
	SectorSize  uint32 // logical
	VirtualSize uint64
	HasParent   bool
	// Present lists the payload blocks this file holds, in order: block i covers the
	// bytes [i*BlockSize, (i+1)*BlockSize) of the virtual disk.
	Present []uint64
}

// ErrVHDXLog means the file has a log Windows has not replayed: its BAT may be
// behind the data, so it cannot be trusted. Attaching the disk once replays it.
var ErrVHDXLog = errors.New("proto: VHDX has a log to replay")

var crc32c = crc32.MakeTable(crc32.Castagnoli)

const (
	vhdxHeader1  = 64 << 10
	vhdxHeader2  = 128 << 10
	vhdxRegions1 = 192 << 10
	vhdxRegions2 = 256 << 10
	vhdxHeaderSz = 4 << 10
	vhdxRegionSz = 64 << 10
	maxVHDXBAT   = 512 << 20 // a BAT of 64 Mi entries: far beyond any disk here
	maxVHDXMeta  = 1 << 20
)

// GUIDs of the regions and metadata items, as in [MS-VHDX].
var (
	guidBAT          = vhdxGUID("2DC27766-F623-4200-9D64-115E9BFD4A08")
	guidMetadata     = vhdxGUID("8B7CA206-4790-4B9A-B8FE-575F050F886E")
	guidFileParams   = vhdxGUID("CAA16737-FA36-4D43-B3B6-33F0AA44E76B")
	guidVirtualSize  = vhdxGUID("2FA54224-CD1B-4876-B211-5DBED83BF4B8")
	guidLogicalSect  = vhdxGUID("8141BF1D-A96F-4709-BA47-F233A8FAAB5F")
	vhdxZeroGUID     = [16]byte{}
	vhdxFileIdentity = []byte("vhdxfile")
)

// vhdxGUID turns the text form into the bytes of a Windows GUID structure.
func vhdxGUID(s string) [16]byte {
	var g [16]byte
	hex := make([]byte, 0, 32)
	for i := 0; i < len(s); i++ {
		if s[i] != '-' {
			hex = append(hex, s[i])
		}
	}
	for i := range g {
		var v byte
		for _, c := range hex[i*2 : i*2+2] {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= c - '0'
			case c >= 'a' && c <= 'f':
				v |= c - 'a' + 10
			case c >= 'A' && c <= 'F':
				v |= c - 'A' + 10
			}
		}
		g[i] = v
	}
	g[0], g[1], g[2], g[3] = g[3], g[2], g[1], g[0]
	g[4], g[5] = g[5], g[4]
	g[6], g[7] = g[7], g[6]
	return g
}

// checksum verifies a structure whose CRC-32C sits at bytes 4..8 and covers it
// all with those bytes zero.
func vhdxChecksum(b []byte) bool {
	want := binary.LittleEndian.Uint32(b[4:])
	c := bytes.Clone(b)
	binary.LittleEndian.PutUint32(c[4:], 0)
	return crc32.Checksum(c, crc32c) == want
}

func readAt(r io.ReaderAt, off int64, n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := r.ReadAt(b, off); err != nil {
		return nil, fmt.Errorf("proto: VHDX at %d: %w", off, err)
	}
	return b, nil
}

// ReadVHDXLayout reads the headers, the region table, the metadata and the BAT.
func ReadVHDXLayout(r io.ReaderAt) (VHDXLayout, error) {
	var l VHDXLayout
	id, err := readAt(r, 0, 8)
	if err != nil {
		return l, err
	}
	if !bytes.Equal(id, vhdxFileIdentity) {
		return l, errors.New("proto: not a VHDX file")
	}

	// The current header has the higher sequence number of the two valid ones.
	var head []byte
	var seq uint64
	for _, off := range []int64{vhdxHeader1, vhdxHeader2} {
		b, err := readAt(r, off, vhdxHeaderSz)
		if err != nil {
			return l, err
		}
		if string(b[:4]) != "head" || !vhdxChecksum(b) {
			continue
		}
		if s := binary.LittleEndian.Uint64(b[8:]); head == nil || s > seq {
			head, seq = b, s
		}
	}
	if head == nil {
		return l, errors.New("proto: VHDX has no valid header")
	}
	if !bytes.Equal(head[48:64], vhdxZeroGUID[:]) {
		return l, ErrVHDXLog
	}
	if v := binary.LittleEndian.Uint16(head[66:]); v != 1 {
		return l, fmt.Errorf("proto: VHDX version %d", v)
	}

	var regions []byte
	for _, off := range []int64{vhdxRegions1, vhdxRegions2} {
		b, err := readAt(r, off, vhdxRegionSz)
		if err != nil {
			return l, err
		}
		if string(b[:4]) == "regi" && vhdxChecksum(b) {
			regions = b
			break
		}
	}
	if regions == nil {
		return l, errors.New("proto: VHDX has no valid region table")
	}
	var batOff, metaOff uint64
	var batLen, metaLen uint32
	count := binary.LittleEndian.Uint32(regions[8:])
	if count > 2047 {
		return l, errors.New("proto: VHDX region table too long")
	}
	for i := range count {
		e := regions[16+32*i:]
		var g [16]byte
		copy(g[:], e[:16])
		switch g {
		case guidBAT:
			batOff, batLen = binary.LittleEndian.Uint64(e[16:]), binary.LittleEndian.Uint32(e[24:])
		case guidMetadata:
			metaOff, metaLen = binary.LittleEndian.Uint64(e[16:]), binary.LittleEndian.Uint32(e[24:])
		default:
			if binary.LittleEndian.Uint32(e[28:])&1 != 0 {
				return l, fmt.Errorf("proto: VHDX has a required region %x unknown here", g)
			}
		}
	}
	if batLen == 0 || metaLen == 0 || batLen > maxVHDXBAT || metaLen > maxVHDXMeta {
		return l, errors.New("proto: VHDX without a usable BAT or metadata")
	}

	meta, err := readAt(r, int64(metaOff), int(metaLen))
	if err != nil {
		return l, err
	}
	if len(meta) < 32 || string(meta[:8]) != "metadata" {
		return l, errors.New("proto: VHDX metadata without its signature")
	}
	items := binary.LittleEndian.Uint16(meta[10:])
	if 32+32*int(items) > len(meta) {
		return l, errors.New("proto: VHDX metadata table too long")
	}
	item := func(g [16]byte, size int) ([]byte, error) {
		for i := range int(items) {
			e := meta[32+32*i:]
			if !bytes.Equal(e[:16], g[:]) {
				continue
			}
			off, n := binary.LittleEndian.Uint32(e[16:]), binary.LittleEndian.Uint32(e[20:])
			if int(n) < size || uint64(off)+uint64(n) > uint64(len(meta)) {
				return nil, errors.New("proto: VHDX metadata item out of place")
			}
			return meta[off : off+n], nil
		}
		return nil, fmt.Errorf("proto: VHDX metadata item %x missing", g)
	}
	params, err := item(guidFileParams, 8)
	if err != nil {
		return l, err
	}
	size, err := item(guidVirtualSize, 8)
	if err != nil {
		return l, err
	}
	sector, err := item(guidLogicalSect, 4)
	if err != nil {
		return l, err
	}
	l.BlockSize = binary.LittleEndian.Uint32(params)
	l.HasParent = binary.LittleEndian.Uint32(params[4:])&2 != 0
	l.VirtualSize = binary.LittleEndian.Uint64(size)
	l.SectorSize = binary.LittleEndian.Uint32(sector)
	if l.BlockSize < 1<<20 || l.BlockSize > 256<<20 || l.BlockSize&(l.BlockSize-1) != 0 ||
		(l.SectorSize != 512 && l.SectorSize != 4096) || l.VirtualSize == 0 {
		return l, fmt.Errorf("proto: VHDX with block %d, sector %d, size %d", l.BlockSize, l.SectorSize, l.VirtualSize)
	}

	// Each chunk of chunkRatio payload entries is followed by one sector bitmap entry.
	chunkRatio := (uint64(1) << 23) * uint64(l.SectorSize) / uint64(l.BlockSize)
	blocks := (l.VirtualSize + uint64(l.BlockSize) - 1) / uint64(l.BlockSize)
	last := blocks - 1
	if need := (last + last/chunkRatio + 1) * 8; need > uint64(batLen) {
		return l, fmt.Errorf("proto: VHDX BAT of %d bytes for %d blocks", batLen, blocks)
	}
	bat, err := readAt(r, int64(batOff), int(batLen))
	if err != nil {
		return l, err
	}
	for i := range blocks {
		entry := binary.LittleEndian.Uint64(bat[(i+i/chunkRatio)*8:])
		// 0 is "not present": the parent holds the data. Any other state is this
		// file's own: data, zeros or a trimmed block, all of them a change.
		if state := entry & 7; state != 0 {
			l.Present = append(l.Present, i)
		}
	}
	return l, nil
}

// ChangedPlaces maps the blocks a differencing disk holds onto the blocks of an
// image laid out by m with blocks of blockSize: for every partition with content,
// the positions any of them touch. Bytes outside such partitions, the GPT for one,
// map to nothing.
func (l VHDXLayout) ChangedPlaces(m Manifest, blockSize uint32) map[uint32]map[uint32]bool {
	out := map[uint32]map[uint32]bool{}
	bs := uint64(blockSize)
	for _, p := range m.Partitions {
		if p.Content != ContentBlocks || p.Size == 0 {
			continue
		}
		set := map[uint32]bool{}
		for _, b := range l.Present {
			start := b * uint64(l.BlockSize)
			end := start + uint64(l.BlockSize)
			lo, hi := max(start, p.Offset), min(end, p.Offset+p.Size)
			if lo >= hi {
				continue
			}
			for pos := (lo - p.Offset) / bs; pos*bs < hi-p.Offset; pos++ {
				set[uint32(pos)] = true
			}
		}
		out[p.Number] = set
	}
	return out
}
