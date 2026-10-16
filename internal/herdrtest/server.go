// Package herdrtest starts throwaway herdr servers for integration tests.
//
// Each server lives under the test's temp dir via XDG_CONFIG_HOME, so its
// sockets, sessions and plugin registry never touch the user's real herdr. The
// environment is scrubbed of HERDR_* (which would otherwise redirect commands
// to the enclosing server) and Claude Code session variables (which make child
// Claude sessions think they are nested and disable transcripts, or point them
// at the parent's CLAUDE_CONFIG_DIR).
package herdrtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bcherb2/herdr-routr/internal/herdr"
)

// Env returns os.Environ() without variables that leak the parent session.
func Env(xdgConfig string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "HERDR_") || strings.HasPrefix(k, "CLAUDE_CODE_") ||
			k == "CLAUDECODE" || k == "CLAUDE_PID" || k == "CLAUDE_EFFORT" || k == "CLAUDE_CONFIG_DIR" ||
			k == "XDG_CONFIG_HOME" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "XDG_CONFIG_HOME="+xdgConfig)
}

// Server is a throwaway herdr server.
type Server struct {
	*herdr.Client
	XDG     string // its XDG_CONFIG_HOME
	Session string
}

// StartServer launches a server and returns its client; see Start.
func StartServer(t *testing.T) *herdr.Client { return Start(t).Client }

// Start launches `herdr --session <name> server` in an isolated config dir.
// The server is killed on test cleanup. Tests are skipped when herdr is not
// installed.
func Start(t *testing.T) *Server {
	t.Helper()
	bin, err := exec.LookPath("herdr")
	if err != nil {
		t.Skip("herdr not installed")
	}
	// Short base dir: Unix socket paths are limited to ~108 bytes.
	xdg, err := os.MkdirTemp("", "routr-it-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(xdg) })
	const session = "it"
	sock := filepath.Join(xdg, "herdr", "sessions", session, "herdr.sock")

	cmd := exec.Command(bin, "--session", session, "server")
	cmd.Env = Env(xdg)
	cmd.Dir = xdg
	logf, err := os.Create(filepath.Join(xdg, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start herdr server: %v", err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		logf.Close()
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(logf.Name())
			t.Fatalf("herdr server socket %s never appeared; log:\n%s", sock, log)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.HasPrefix(sock, xdg) {
		t.Fatalf("refusing non-isolated socket %s", sock)
	}
	return &Server{Client: herdr.New(sock), XDG: xdg, Session: session}
}
