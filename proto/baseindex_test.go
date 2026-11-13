package proto

import (
	"strings"
	"testing"
)

func layoutManifest(windowsSize uint64) Manifest {
	return Manifest{
		Version: ManifestVersion, DiskSize: 64 << 30, SectorSize: 512, DiskGUID: "11111111-2222-4333-8444-555555555555",
		Partitions: []Partition{
			{Number: 1, TypeGUID: TypeESP, GUID: "aaaaaaaa-0000-4000-8000-000000000001", Offset: 1 << 20, Size: 100 << 20, Policy: PolicyFixed, Content: ContentBlocks},
			{Number: 2, TypeGUID: TypeMSR, GUID: "aaaaaaaa-0000-4000-8000-000000000002", Offset: 101 << 20, Size: 16 << 20, Policy: PolicyFixed, Content: ContentNone},
			{Number: 3, TypeGUID: TypeBasicData, GUID: "aaaaaaaa-0000-4000-8000-000000000003", Offset: 117 << 20, Size: windowsSize, Policy: PolicyFill, Content: ContentBlocks},
			{Number: 4, TypeGUID: "de94bba4-06d1-4d40-a16a-bfd50179d6ac", GUID: "aaaaaaaa-0000-4000-8000-000000000004", Offset: 117<<20 + windowsSize, Size: 700 << 20, Policy: PolicyFixed, Content: ContentBlocks},
		},
	}
}

func TestBaseIndexRoundTrip(t *testing.T) {
	x := BaseIndex{Image: "win11", Version: 7, BlockSize: DefaultBlockSize, Manifest: layoutManifest(30 << 30),
		Entries: []DeployEntry{{Partition: 3, Position: 0, Hash: Sum{1}}, {Partition: 3, Position: 9}}}
	b, err := x.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseBaseIndex(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Image != "win11" || got.Version != 7 || len(got.Entries) != 2 || got.Entries[0].Hash != (Sum{1}) ||
		!got.Entries[1].Zero() || !SameLayout(got.Manifest, x.Manifest) {
		t.Fatalf("got %+v", got)
	}
	for _, bad := range [][]byte{nil, []byte("LVBI"), append([]byte("XXXX"), b[4:]...), b[:len(b)-1]} {
		if _, err := ParseBaseIndex(bad); err == nil {
			t.Fatalf("accepted %q", bad[:min(len(bad), 8)])
		}
	}
}

// TestSameLayoutAfterAnotherShrink: two versions of one image, the volume shrunk
// to different sizes by their clones, land in the same places on one disk.
func TestSameLayoutAfterAnotherShrink(t *testing.T) {
	const first, end = 1 << 20, 64<<30 - 1<<20
	a, err := layoutManifest(30<<30).Fit(first, end)
	if err != nil {
		t.Fatal(err)
	}
	b, err := layoutManifest(34<<30).Fit(first, end)
	if err != nil {
		t.Fatal(err)
	}
	if !SameLayout(a, b) {
		t.Fatalf("different:\n%+v\n%+v", a.Partitions, b.Partitions)
	}

	// Anything that moves a partition or changes who it is breaks it.
	changes := map[string]func(*Manifest){
		"disk size":   func(m *Manifest) { m.DiskSize += 1 << 30 },
		"disk guid":   func(m *Manifest) { m.DiskGUID = strings.Replace(m.DiskGUID, "1111", "9999", 1) },
		"offset":      func(m *Manifest) { m.Partitions[0].Offset += 1 << 20 },
		"size":        func(m *Manifest) { m.Partitions[3].Size += 1 << 20 },
		"guid":        func(m *Manifest) { m.Partitions[2].GUID = "bbbbbbbb-0000-4000-8000-000000000003" },
		"type":        func(m *Manifest) { m.Partitions[1].TypeGUID = TypeBasicData },
		"one less":    func(m *Manifest) { m.Partitions = m.Partitions[:3] },
		"content":     func(m *Manifest) { m.Partitions[1].Content = ContentBlocks },
		"sector size": func(m *Manifest) { m.SectorSize = 4096 },
	}
	for name, change := range changes {
		c := b
		c.Partitions = append([]Partition(nil), b.Partitions...)
		change(&c)
		if SameLayout(a, c) {
			t.Errorf("%s: still the same layout", name)
		}
	}
	upper := b
	upper.DiskGUID = strings.ToUpper(b.DiskGUID)
	if !SameLayout(a, upper) {
		t.Error("GUIDs differ only in case")
	}
}
