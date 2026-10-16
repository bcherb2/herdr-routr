package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMarkedPathSkipsRCNoise(t *testing.T) {
	out := "welcome banner\n[ERROR]: gitstatus failed\n\n" + pathMarker + "/a:/b\n"
	if p, ok := markedPath(out); !ok || p != "/a:/b" {
		t.Fatalf("got %q %v", p, ok)
	}
	if _, ok := markedPath("no marker here\n"); ok {
		t.Fatal("found a PATH without the marker")
	}
}

func TestMergePathKeepsOrderAndDropsDuplicates(t *testing.T) {
	if got := mergePath("/shell:/usr/bin", "/usr/bin:/inherited::/shell"); got != "/shell:/usr/bin:/inherited" {
		t.Fatalf("got %q", got)
	}
}

func TestAdoptShellPathUsesInteractiveShell(t *testing.T) {
	dir := t.TempDir()
	shell := filepath.Join(dir, "fakesh")
	// Stands in for a shell whose rc file adds a directory and prints noise.
	script := "#!/bin/sh\necho rc noise\nPATH=/from/rc:$PATH\nexport PATH\nexec /bin/sh -c \"$2\" \"$3\"\n"
	if err := os.WriteFile(shell, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", shell)
	t.Setenv("PATH", "/usr/bin:/bin")
	if err := adoptShellPath(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("PATH"); !strings.HasPrefix(got, "/from/rc:") || !strings.Contains(got, "/usr/bin") {
		t.Fatalf("PATH = %q", got)
	}
}
