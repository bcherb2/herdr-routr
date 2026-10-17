package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/bcherb2/herdr-routr/internal/config"
)

// configFile is one config file routr reads, and whether it exists.
type configFile struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
}

// configCmd: the config files routr reads; `example` prints the commented
// template; `init` writes it as the user config when there is none.
func (a *app) configCmd(args []string) error {
	fs, c := a.flags("config")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	sub := ""
	if len(pos) > 0 {
		sub = pos[0]
	}
	if len(pos) > 1 || (sub != "" && sub != "init" && sub != "example") {
		return usagef("routr config [init|example]: init, example, or nothing to list the files")
	}
	user := userConfig(a.getenv)
	switch sub {
	case "example":
		if c.json {
			return a.printJSON(map[string]string{"text": config.Example})
		}
		_, err := fmt.Fprint(a.stdout, config.Example)
		return err
	case "init":
		created, err := writeNew(user, config.Example)
		if err != nil {
			return err
		}
		if c.json {
			return a.printJSON(map[string]any{"path": user, "created": created})
		}
		if created {
			fmt.Fprintf(a.stdout, "routr: wrote %s: every key commented out; uncomment one to change it\n", user)
		} else {
			fmt.Fprintf(a.stdout, "routr: %s exists; left alone (routr config example prints the template)\n", user)
		}
		return nil
	}
	p, err := a.paths(c)
	if err != nil {
		return err
	}
	files := map[string]configFile{"user": {Path: user, Exists: exists(user)}, "repo": {Path: p.Config(), Exists: exists(p.Config())}}
	if c.json {
		return a.printJSON(files)
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	for _, k := range []struct{ name, none string }{
		{"user", "none; routr config init writes a commented one"},
		{"repo", "none; optional, laid over the user's key by key"},
	} {
		note := ""
		if !files[k.name].Exists {
			note = "(" + k.none + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", k.name, files[k.name].Path, note)
	}
	return tw.Flush()
}

// writeNew writes text to path, creating its directory, unless path exists.
func writeNew(path, text string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return false, err
	}
	return true, f.Close()
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
