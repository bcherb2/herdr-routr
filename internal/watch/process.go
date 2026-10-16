package watch

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/bcherb2/herdr-routr/internal/registry"
)

// ErrRunning is returned by Hold when another watcher has the repo.
var ErrRunning = errors.New("a watcher is already running for this repo")

// Hold takes the repo's watcher lock (state/watch.lock) for the life of the
// process; release gives it up. The lock is an flock, so it is freed when
// the process dies however it dies.
func Hold(p registry.Paths) (release func(), err error) {
	if err := p.Init(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p.WatchLock(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrRunning
		}
		return nil, fmt.Errorf("lock %s: %w", p.WatchLock(), err)
	}
	f.Truncate(0)
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// Running reports whether a watcher holds the repo's lock.
func Running(p registry.Paths) bool {
	release, err := Hold(p)
	if err != nil {
		return errors.Is(err, ErrRunning)
	}
	release()
	return false
}

// maxLog is the size past which the log starts afresh (the old one is kept
// as watch.log.1).
const maxLog = 1 << 20

// LogTo is where a watcher logs, given its stderr: when that is the repo's
// watch.log (a watcher Spawn started), a writer that caps it at maxLog,
// copying it to watch.log.1 and emptying it, so a long-lived watcher cannot
// fill the disk. The file stays the process's stderr, so a crash still lands
// in it. Any other stderr (a terminal) is returned as is.
func LogTo(p registry.Paths, stderr io.Writer) io.Writer {
	f, ok := stderr.(*os.File)
	if !ok {
		return stderr
	}
	fi, err := f.Stat()
	if err != nil {
		return stderr
	}
	if li, err := os.Stat(p.WatchLog()); err != nil || !os.SameFile(fi, li) {
		return stderr
	}
	return &cappedLog{f: f, path: p.WatchLog()}
}

// Logf appends a line to the repo's watch.log, as the watcher logs, for a
// command that acts on workers itself (`routr answer`).
func Logf(p registry.Paths, format string, args ...any) error {
	f, err := os.OpenFile(p.WatchLog(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	log.New(f, "", log.LstdFlags).Printf(format, args...)
	return f.Close()
}

type cappedLog struct {
	mu   sync.Mutex
	f    *os.File // opened for appending
	path string
}

func (l *cappedLog) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if fi, err := l.f.Stat(); err == nil && fi.Size()+int64(len(b)) > maxLog {
		if old, err := os.ReadFile(l.path); err == nil && registry.WriteFile(l.path+".1", old) == nil {
			l.f.Truncate(0) // best effort: if it fails, the log keeps growing
		}
	}
	return l.f.Write(b)
}

// Spawn starts `bin watch --foreground` for the repo, detached in its own
// session with output to state/watch.log, unless a watcher is running. Two
// racing spawns are harmless: the loser fails to take the lock and exits.
func Spawn(p registry.Paths, bin string) error {
	if Running(p) {
		return nil
	}
	if fi, err := os.Stat(p.WatchLog()); err == nil && fi.Size() > maxLog {
		os.Rename(p.WatchLog(), p.WatchLog()+".1")
	}
	logf, err := os.OpenFile(p.WatchLog(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(bin, "watch", "--foreground")
	cmd.Env = append(os.Environ(), registry.StateDirEnv+"="+p.Dir)
	cmd.Dir = p.Root()
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the watcher (%s watch): %w", bin, err)
	}
	return cmd.Process.Release()
}
