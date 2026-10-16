package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// DoneRecord is done.json: what `routr done` reports. Workers never write
// the registry; they write this into their run dir and the watcher picks
// it up.
type DoneRecord struct {
	Output  string    `json:"output,omitempty"` // absolute path
	Verdict Verdict   `json:"verdict,omitempty"`
	Message string    `json:"message,omitempty"`
	At      time.Time `json:"at"`
}

// WriteDone writes runDir/done.json atomically.
func WriteDone(runDir string, d DoneRecord) error {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return err
	}
	return WriteFile(filepath.Join(runDir, DoneFile), append(b, '\n'))
}

// TakeDone reads runDir/done.json and renames it to done.<n>.json, so a later
// round's `routr done` is seen fresh. ok is false when there is none; an
// unreadable one is still taken (ok, with an error) so it is not read again.
func TakeDone(runDir string) (d DoneRecord, ok bool, err error) {
	path := filepath.Join(runDir, DoneFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	bad := json.Unmarshal(b, &d)
	if err := os.Rename(path, nextAside(runDir)); err != nil {
		return d, false, err
	}
	if bad != nil {
		return DoneRecord{}, true, fmt.Errorf("%s: %w", path, bad)
	}
	return d, true, nil
}

func nextAside(runDir string) string {
	for n := 1; ; n++ {
		aside := filepath.Join(runDir, fmt.Sprintf("done.%d.json", n))
		if _, err := os.Lstat(aside); errors.Is(err, fs.ErrNotExist) {
			return aside
		}
	}
}

// HasDone reports whether runDir has a done.json waiting.
func HasDone(runDir string) bool {
	_, err := os.Stat(filepath.Join(runDir, DoneFile))
	return err == nil
}

// WriteFile replaces path atomically, creating its directory.
func WriteFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
