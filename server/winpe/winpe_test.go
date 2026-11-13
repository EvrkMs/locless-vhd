package winpe

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestFollowPrefersTheErrorLine(t *testing.T) {
	b := &Builder{Log: quiet()}
	out := "12:00:00 source x\nerror: DISM needs administrator rights\n\nsomething after\n"
	if got := b.follow(strings.NewReader(out)); got != "DISM needs administrator rights" {
		t.Fatalf("got %q", got)
	}
	if got := b.follow(strings.NewReader("one\ntwo\n")); got != "two" {
		t.Fatalf("without an error line: %q", got)
	}
}

// TestRunLeavesAnExistingFile: an operator's own boot.wim is never replaced.
func TestRunLeavesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "boot.wim")
	if err := os.WriteFile(out, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &Builder{Out: out, Work: filepath.Join(dir, "work"), Log: quiet()}
	b.Run(context.Background())
	if st := b.Status(); st.Building || st.Error != "" {
		t.Fatalf("status %+v", st)
	}
	if _, err := os.Stat(b.Work); !os.IsNotExist(err) {
		t.Fatalf("work folder made: %v", err)
	}
}

func TestScriptIsASCII(t *testing.T) {
	// Windows PowerShell reads a script without a BOM in the ANSI code page.
	for i, c := range script {
		if c > 127 {
			t.Fatalf("byte %d is not ASCII", i)
		}
	}
}
