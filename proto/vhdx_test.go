package proto

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"slices"
	"testing"
)

// childVHDX is a differencing disk Windows made itself: diskpart created a
// dynamic parent of 200 MiB and a child on top, and the child got 1 MiB at
// 40 MiB and 4 KiB at 150 MiB + 4 KiB. Its blocks are 2 MiB, so the child holds
// blocks 20 and 75 and nothing else.
func childVHDX(t *testing.T) []byte {
	t.Helper()
	f, err := os.Open("testdata/child.vhdx.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVHDXLayoutOfAWindowsDisk(t *testing.T) {
	l, err := ReadVHDXLayout(bytes.NewReader(childVHDX(t)))
	if err != nil {
		t.Fatal(err)
	}
	if l.BlockSize != 2<<20 || l.SectorSize != 512 || l.VirtualSize != 200<<20 || !l.HasParent {
		t.Fatalf("layout %+v", l)
	}
	if !slices.Equal(l.Present, []uint64{20, 75}) {
		t.Fatalf("present %v, want [20 75]", l.Present)
	}
}

func TestVHDXRefusesWhatItCannotTrust(t *testing.T) {
	good := childVHDX(t)
	cases := map[string]func(b []byte){
		"not vhdx": func(b []byte) { copy(b, "notvhdx!") },
		// Both headers broken: no sequence to trust.
		"headers": func(b []byte) { b[vhdxHeader1+20]++; b[vhdxHeader2+20]++ },
		"regions": func(b []byte) { b[vhdxRegions1+20]++; b[vhdxRegions2+20]++ },
	}
	for name, spoil := range cases {
		b := bytes.Clone(good)
		spoil(b)
		if _, err := ReadVHDXLayout(bytes.NewReader(b)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// A log not yet replayed: the BAT may be behind the data.
	b := bytes.Clone(good)
	for _, off := range []int{vhdxHeader1, vhdxHeader2} {
		h := b[off : off+vhdxHeaderSz]
		if string(h[:4]) != "head" {
			continue
		}
		h[48] = 1
		binary.LittleEndian.PutUint32(h[4:], 0)
		binary.LittleEndian.PutUint32(h[4:], crc32.Checksum(h, crc32c))
	}
	if _, err := ReadVHDXLayout(bytes.NewReader(b)); !errors.Is(err, ErrVHDXLog) {
		t.Fatalf("log: %v", err)
	}
	if _, err := ReadVHDXLayout(bytes.NewReader(good[:vhdxRegions1])); err == nil {
		t.Fatal("a cut file was accepted")
	}
}

// TestChangedPlaces: VHDX blocks of 2 MiB over image blocks of 1 MiB, with a
// partition that does not start on a VHDX block.
func TestChangedPlaces(t *testing.T) {
	l := VHDXLayout{BlockSize: 2 << 20, Present: []uint64{0, 3, 10}}
	m := Manifest{Partitions: []Partition{
		{Number: 1, Offset: 1 << 20, Size: 2 << 20, Content: ContentBlocks},  // 1..3 MiB
		{Number: 2, Offset: 3 << 20, Size: 1 << 20, Content: ContentNone},    // MSR: nothing
		{Number: 3, Offset: 5 << 20, Size: 10 << 20, Content: ContentBlocks}, // 5..15 MiB
	}}
	got := l.ChangedPlaces(m, 1<<20)
	want := map[uint32][]uint32{
		1: {0},    // block 0 covers 0..2 MiB: the first MiB of partition 1
		3: {1, 2}, // block 3 covers 6..8 MiB: positions 1 and 2; block 10 is past the end
	}
	for part, positions := range want {
		var have []uint32
		for p := range got[part] {
			have = append(have, p)
		}
		slices.Sort(have)
		if !slices.Equal(have, positions) {
			t.Errorf("partition %d: %v, want %v", part, have, positions)
		}
	}
	if _, ok := got[2]; ok {
		t.Error("a partition without content got places")
	}
}
