// Package winpe makes the WinPE the agent boots from when the server has none:
// build.ps1 takes the Windows Recovery Environment of the server itself and cuts
// it down. Nothing has to be copied from an ISO, and nothing is installed.
package winpe

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

//go:embed build.ps1
var script []byte

// Builder builds the file once, in the background, and tells how it goes.
type Builder struct {
	Out  string // the boot.wim to make
	Work string // empty space for a copy of WinRE and the mount; a short path
	Log  *slog.Logger

	mu      sync.Mutex
	running bool
	failure string
}

// Status is what the panel shows: whether a build runs and why the last one failed.
type Status struct {
	Building bool   `json:"building,omitempty"`
	Error    string `json:"error,omitempty"`
}

func (b *Builder) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Status{Building: b.running, Error: b.failure}
}

// Run builds Out unless it exists or a build runs already. It returns when
// the build ends.
func (b *Builder) Run(ctx context.Context) {
	if _, err := os.Stat(b.Out); err == nil {
		return
	}
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return
	}
	b.running, b.failure = true, ""
	b.mu.Unlock()

	b.Log.Info("WinPE is missing: building it from the WinRE of this server", "out", b.Out)
	err := b.build(ctx)
	b.mu.Lock()
	b.running = false
	if err != nil {
		b.failure = err.Error()
	}
	b.mu.Unlock()
	if err != nil {
		b.Log.Error("WinPE not built", "err", err)
		return
	}
	b.Log.Info("WinPE built", "out", b.Out)
}

func (b *Builder) build(ctx context.Context) error {
	if err := os.MkdirAll(b.Work, 0o755); err != nil {
		return err
	}
	path := filepath.Join(b.Work, "build.ps1")
	if err := os.WriteFile(path, script, 0o644); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-File", path, "-Out", b.Out, "-Work", b.Work)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	last := b.follow(out)
	if err := cmd.Wait(); err != nil {
		if last == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, last)
	}
	if _, err := os.Stat(b.Out); err != nil {
		return errors.New("the script ended without the file")
	}
	return nil
}

// follow logs the lines of the script and returns its error line, or the last
// line if it printed none.
func (b *Builder) follow(r io.Reader) string {
	var last, failure string
	lines := bufio.NewScanner(r)
	for lines.Scan() {
		line := strings.TrimSpace(lines.Text())
		if line == "" {
			continue
		}
		b.Log.Info("WinPE build", "line", line)
		last = line
		if msg, ok := strings.CutPrefix(line, "error: "); ok && failure == "" {
			failure = msg
		}
	}
	if failure != "" {
		return failure
	}
	return last
}
