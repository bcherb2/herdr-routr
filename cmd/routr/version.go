package main

import (
	"fmt"
	"os/exec"
	"runtime/debug"
	"strings"
)

// srcDir is the checkout this binary was built from; install.sh sets it
// with -ldflags "-X main.srcDir=…". Empty for any other build.
var srcDir string

// buildInfo is what this binary knows of its own build. go build stamps the
// VCS fields when it builds inside a git checkout.
type buildInfo struct {
	Revision string `json:"revision"`       // vcs.revision; empty when not stamped
	Time     string `json:"time,omitempty"` // vcs.time, the commit's time
	Modified bool   `json:"modified"`       // vcs.modified: built with uncommitted changes
	Go       string `json:"go"`             // the Go version it was built with
	Src      string `json:"src,omitempty"`  // srcDir
}

func readBuild() buildInfo {
	b := buildInfo{Src: srcDir}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return b
	}
	b.Go = bi.GoVersion
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			b.Revision = s.Value
		case "vcs.time":
			b.Time = s.Value
		case "vcs.modified":
			b.Modified = s.Value == "true"
		}
	}
	return b
}

// short is the revision as the watcher logs it: 12 hex digits, +dirty when
// built with uncommitted changes, or "unknown".
func (b buildInfo) short() string {
	if b.Revision == "" {
		return "unknown"
	}
	s := b.Revision[:min(12, len(b.Revision))]
	if b.Modified {
		s += "+dirty"
	}
	return s
}

// version prints the build this binary was made from.
func (a *app) version(args []string) error {
	fs, c := a.flags("version")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if c.json {
		return a.printJSON(a.build)
	}
	fmt.Fprintf(a.stdout, "routr %s", a.build.short())
	if a.build.Time != "" {
		fmt.Fprintf(a.stdout, " (%s)", a.build.Time)
	}
	if a.build.Go != "" {
		fmt.Fprintf(a.stdout, " %s", a.build.Go)
	}
	fmt.Fprintln(a.stdout)
	if a.build.Src != "" {
		fmt.Fprintln(a.stdout, "source:", a.build.Src)
	}
	return nil
}

// buildPaths are the paths whose changes make a new binary; the skill and
// the Pi extension are read live from the checkout.
var buildPaths = []string{"cmd", "internal", "go.mod", "go.sum"}

// checkBuild is doctor's build line: whether this binary was built from
// its checkout's HEAD, as far as the checkout's own history tells, and
// without uncommitted changes.
func (a *app) checkBuild(add func(name, status, detail, fix string)) {
	b := a.build
	if b.Revision == "" {
		add("build", "ok", "routr carries no VCS revision (not built by go build in a git checkout); cannot tell whether it is current", "")
		return
	}
	name, rebuild := "routr "+b.short(), "commit them and run install.sh again"
	if b.Src != "" {
		rebuild = "run " + b.Src + "/install.sh"
	}
	dirty := ""
	if b.Modified {
		dirty = ", and was built with uncommitted changes"
	}
	if b.Src == "" {
		if b.Modified {
			add("build", "warn", name+" was built with uncommitted changes", rebuild)
			return
		}
		add("build", "ok", name+" (built outside install.sh: no source checkout to compare with)", "")
		return
	}
	cmd := exec.CommandContext(a.ctx, "git", append([]string{"-C", b.Src, "log", "--oneline", b.Revision + "..HEAD", "--"}, buildPaths...)...)
	out, err := cmd.Output()
	switch lines := strings.Split(strings.TrimSpace(string(out)), "\n"); {
	case err != nil:
		add("build", "warn", fmt.Sprintf("cannot compare %s with %s: %v", name, b.Src, gitErr(err)), rebuild)
	case lines[0] != "":
		add("build", "warn", fmt.Sprintf("%s is %d commits behind %s (latest: %s)%s; the skill and Pi extension there are newer than this binary",
			name, len(lines), b.Src, lines[0], dirty), rebuild)
	case b.Modified:
		add("build", "warn", name+" matches "+b.Src+" but was built with uncommitted changes", rebuild)
	default:
		add("build", "ok", name+" is current with "+b.Src, "")
	}
}

// gitErr is a failed git command's own message, when it gave one.
func gitErr(err error) error {
	if ee, ok := err.(*exec.ExitError); ok {
		if msg := strings.TrimSpace(string(ee.Stderr)); msg != "" {
			line, _, _ := strings.Cut(msg, "\n")
			return fmt.Errorf("%s", line)
		}
	}
	return err
}
