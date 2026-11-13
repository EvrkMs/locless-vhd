package proto

import (
	"errors"
	"testing"
)

func TestStoreLayout(t *testing.T) {
	const gib = 1 << 30
	// A 128 GB disk as Windows reports its usable area: 17 KiB in, 16.5 KiB short of the end.
	first, end := uint64(17408), uint64(128_000_000_000-16896)
	parts, err := StoreLayout(first, end, 512)
	if err != nil {
		t.Fatalf("StoreLayout: %v", err)
	}
	if !IsStoreLayout(parts) {
		t.Fatalf("not a store layout: %+v", parts)
	}
	prev := first
	for i, p := range parts {
		if p.Offset%storeAlign != 0 || p.Offset < prev {
			t.Errorf("partition %d at %d, previous end %d", i+1, p.Offset, prev)
		}
		if !validGUID(p.GUID) {
			t.Errorf("partition %d guid %q", i+1, p.GUID)
		}
		prev = p.Offset + p.Size
	}
	if prev > end || end-prev >= storeAlign {
		t.Errorf("store ends at %d, usable area at %d", prev, end)
	}
	store := parts[2]
	if store.Attributes != AttrNoDriveLetter || store.Size < 100*gib {
		t.Errorf("store: %+v", store)
	}
	if parts[0].GUID == parts[1].GUID || parts[1].GUID == parts[2].GUID {
		t.Error("partitions share a guid")
	}
}

func TestStoreLayoutRefusesSmallDisks(t *testing.T) {
	for _, tc := range []struct {
		name       string
		first, end uint64
	}{
		{"empty", 1 << 20, 1 << 20},
		{"reversed", 2 << 20, 1 << 20},
		{"8 GiB", 17408, 8 << 30},
	} {
		if _, err := StoreLayout(tc.first, tc.end, 512); !errors.Is(err, ErrDiskTooSmall) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	if _, err := StoreLayout(17408, 64<<30, 1000); err == nil {
		t.Error("sector of 1000 bytes accepted")
	}
}

func TestIsStoreLayout(t *testing.T) {
	parts, err := StoreLayout(1<<20, 64<<30, 4096)
	if err != nil {
		t.Fatalf("StoreLayout: %v", err)
	}
	upper := append([]Partition(nil), parts...)
	upper[0].TypeGUID = "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"
	if !IsStoreLayout(upper) {
		t.Error("upper-case GUID refused")
	}
	renamed := append([]Partition(nil), parts...)
	renamed[2].Name = "Basic data partition"
	if IsStoreLayout(renamed) || IsStoreLayout(parts[:2]) || IsStoreLayout(append(parts, parts[2])) {
		t.Error("a foreign table taken for a store")
	}
}

func TestStoreNeed(t *testing.T) {
	const gib = 1 << 30
	if got := StoreNeed(64*gib, 16*gib); got != (64+8+16+4+1)*gib {
		t.Fatalf("StoreNeed: %d GiB", got/gib)
	}
	if got := StoreNeed(200*gib, 0); got != (200+20+4+1)*gib {
		t.Fatalf("StoreNeed with a tenth: %d GiB", got/gib)
	}
}

func TestAutoVHDSize(t *testing.T) {
	const gib = 1 << 30
	for _, store := range []uint64{20 * gib, 60 * gib, 222 * gib, 930 * gib, 1863*gib + 12345} {
		size := AutoVHDSize(store, 16*gib)
		fits := size == 0 || StoreNeed(size, 16*gib) <= store
		if size%gib != 0 || !fits || StoreNeed(size+gib, 16*gib) <= store {
			t.Errorf("store %d GiB: size %d GiB is not the largest that fits", store/gib, size/gib)
		}
	}
	if AutoVHDSize(10*gib, 16*gib) != 0 {
		t.Error("a store smaller than the reserve gave a size")
	}
	// The 240 GB disk of PC08: the store is about 222 GiB.
	if got := AutoVHDSize(222*gib, 16*gib) / gib; got < 180 {
		t.Errorf("PC08 would get %d GiB", got)
	}
}

func TestValidUserName(t *testing.T) {
	for name, want := range map[string]bool{
		"Player": true, "Игрок 1": true, "a.b-c_d": true,
		"": false, "trailing.": false, "trailing ": false, "a/b": false, `a\b`: false,
		"a@b": false, "123456789012345678901": false,
	} {
		if got := ValidUserName(name); got != want {
			t.Errorf("%q: %v, want %v", name, got, want)
		}
	}
	if !(&Accounts{}).Empty() || !(*Accounts)(nil).Empty() || (&Accounts{PlayerUser: "p"}).Empty() {
		t.Error("Empty")
	}
}
