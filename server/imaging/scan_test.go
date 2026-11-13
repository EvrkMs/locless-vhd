package imaging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copyFolder copies the files of an image folder, as an operator would move an image.
func copyFolder(t *testing.T, from, to string, names ...string) {
	t.Helper()
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(from, name))
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if err := os.WriteFile(filepath.Join(to, name), b, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
}

func TestReadFolderFindsTheChain(t *testing.T) {
	m, _, images := setup(t)
	threeVersions(t, m)
	dir := filepath.Join(t.TempDir(), "moved")
	copyFolder(t, filepath.Join(images, "win11"), dir, "v1.ldisk", "v2.ldelta", "v3.ldelta")
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644)

	found, err := ReadFolder(dir)
	if err != nil {
		t.Fatalf("ReadFolder: %v", err)
	}
	if len(found.Versions) != 3 || len(found.Skipped) != 0 {
		t.Fatalf("found %+v", found)
	}
	for i, v := range found.Versions {
		if v.Number != i+1 || v.Parent != i || v.Size == 0 {
			t.Fatalf("version %d: %+v", i, v)
		}
	}
}

func TestReadFolderStopsAtABrokenLink(t *testing.T) {
	m, _, images := setup(t)
	threeVersions(t, m)
	dir := filepath.Join(t.TempDir(), "moved")
	copyFolder(t, filepath.Join(images, "win11"), dir, "v1.ldisk", "v3.ldelta") // v2 lost on the way

	found, err := ReadFolder(dir)
	if err != nil {
		t.Fatalf("ReadFolder: %v", err)
	}
	if len(found.Versions) != 1 || found.Versions[0].File != "v1.ldisk" {
		t.Fatalf("versions %+v", found.Versions)
	}
	if len(found.Skipped) != 1 || !strings.HasPrefix(found.Skipped[0], "v3.ldelta") {
		t.Fatalf("skipped %v", found.Skipped)
	}
}

func TestReadFolderNeedsAFullVersion(t *testing.T) {
	m, _, images := setup(t)
	threeVersions(t, m)
	dir := filepath.Join(t.TempDir(), "moved")
	copyFolder(t, filepath.Join(images, "win11"), dir, "v2.ldelta")
	found, err := ReadFolder(dir)
	if err != nil || len(found.Versions) != 0 || len(found.Skipped) != 1 {
		t.Fatalf("found %+v, err %v", found, err)
	}
	os.WriteFile(filepath.Join(dir, "v1.ldisk"), []byte("not an image"), 0o644)
	if found, _ := ReadFolder(dir); len(found.Versions) != 0 {
		t.Fatalf("a broken base made a chain: %+v", found)
	}
}
