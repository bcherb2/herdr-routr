package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bcherb2/herdr-routr/internal/config"
)

const pathMarker = "__ROUTR_PATH__"

// adoptShellPath sets PATH to what the user's interactive login shell sets,
// keeping inherited entries it lacks. herdr starts plugin panes with the
// server's environment, which often lacks the rc-file PATH that harness panes
// (interactive shells) get, so `claude`/`codex` would look uninstalled.
func adoptShellPath(ctx context.Context) error {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// rc files may print banners or errors; the marker line isolates PATH.
	out, err := exec.CommandContext(ctx, shell, "-ilc", `printf '\n%s%s\n' "$0" "$PATH"`, pathMarker).Output()
	if err != nil {
		return err
	}
	p, ok := markedPath(string(out))
	if !ok {
		return errors.New("shell printed no PATH")
	}
	return os.Setenv("PATH", mergePath(p, os.Getenv("PATH")))
}

func markedPath(out string) (string, bool) {
	for line := range strings.Lines(out) {
		if p, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), pathMarker); ok && p != "" {
			return p, true
		}
	}
	return "", false
}

// mergePath is first's entries, then second's that first lacks.
func mergePath(first, second string) string {
	seen := map[string]bool{}
	var out []string
	for _, dir := range filepath.SplitList(first + string(filepath.ListSeparator) + second) {
		if dir != "" && !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	return strings.Join(out, string(filepath.ListSeparator))
}

// withShellPath retries a "not found on PATH" readiness failure with the
// login shell's PATH (adopted once per process): routr may run from a
// process whose PATH lacks the rc-file entries.
func withShellPath(ready config.ReadyFunc) config.ReadyFunc {
	tried, adopted := false, false
	return func(ctx context.Context, harness string) error {
		err := ready(ctx, harness)
		if err == nil || !strings.Contains(err.Error(), "not found on PATH") {
			return err
		}
		if !tried {
			tried, adopted = true, adoptShellPath(ctx) == nil
		}
		if !adopted {
			return err
		}
		return ready(ctx, harness)
	}
}
