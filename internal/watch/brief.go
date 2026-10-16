package watch

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bcherb2/herdr-routr/internal/flow"
	"github.com/bcherb2/herdr-routr/internal/registry"
	"github.com/bcherb2/herdr-routr/internal/worker"
)

// Refs builds what a brief of wk may refer to, from the registry
// (flow.RefsFor) and its chain's work directory, with paths made relative
// to dir where they lie inside it; dir "" keeps them absolute.
func Refs(wk registry.Worker, all []registry.Worker, p registry.Paths, dir string) flow.Refs {
	abs := make([]registry.Worker, len(all))
	for i, o := range all { // an output is relative to the cwd of the worker that wrote it
		o.Output = absIn(o.Cwd, o.Output)
		abs[i] = o
	}
	r := flow.RefsFor(wk, abs, p.FinalPath)
	r.Work = relTo(dir, p.WorkDir(r.Chain))
	if r.Item != nil { // as matched: relative to wk's cwd unless the glob was absolute
		r.Item = &flow.Item{Path: relTo(dir, absIn(wk.Cwd, r.Item.Path)), Name: r.Item.Name}
	}
	for id, s := range r.Steps {
		s.Output = relTo(dir, s.Output)
		s.Final = relTo(dir, s.Final)
		for i, o := range s.Outputs {
			s.Outputs[i] = relTo(dir, o)
		}
		r.Steps[id] = s
	}
	return r
}

func absIn(dir, path string) string {
	if path == "" || filepath.IsAbs(path) || dir == "" {
		return path
	}
	return filepath.Join(dir, path)
}

// relTo is path relative to dir when it lies inside dir, else absolute.
func relTo(dir, path string) string {
	if dir == "" || !filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return path
	}
	return rel
}

// Render fills wk's brief from the registry, every path in it absolute.
// Claude's file tools take absolute paths, and Haiku made a relative
// `.routr/work/…` into `~/.routr/work/…`, outside the repo, where even
// acceptEdits asks before writing.
func Render(wk registry.Worker, all []registry.Worker, p registry.Paths) (string, error) {
	return flow.Render(wk.Brief, Refs(wk, all, p, ""))
}

// Compose is the first prompt a worker gets: its role's preamble, the
// rendered brief, and how to report back with `routr done` (bin is the
// routr binary).
func Compose(wk registry.Worker, p registry.Paths, brief, bin string) string {
	var b strings.Builder
	if pre := strings.TrimSpace(wk.Preamble); pre != "" {
		b.WriteString(pre + "\n\n")
	}
	b.WriteString(strings.TrimSpace(brief) + "\n\n")
	b.WriteString(Finish(wk, p, bin))
	return b.String()
}

// Revised is the prompt a worker gets when a review loop sends it round
// wk.Round: it resumes its own session, so no preamble.
func Revised(wk registry.Worker, p registry.Paths, brief, bin string) string {
	return fmt.Sprintf("The work was revised; review it again (round %d), as your brief says:\n\n%s\n\n%s",
		wk.Round, strings.TrimSpace(brief), Finish(wk, p, bin))
}

// Finish tells the worker how to end, the one place that says it, for
// every harness and every prompt (Compose, Revised, on_fail's follow-up).
// It spells routr done as worker.DoneCommand does so that Claude's allow
// rule matches it, with the summary in single quotes: a $ or a backtick in
// double quotes is shell expansion, and Claude asks before any command
// with one, whatever its rules allow.
func Finish(wk registry.Worker, p registry.Paths, bin string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\nYou are routr worker %d (role %s).", wk.ID, wk.Role)
	out := "FILE"
	if wk.Output != "" {
		out = absIn(wk.Cwd, wk.Output) // absolute, as in Render
		fmt.Fprintf(&b, " Write your result to %s.", out)
	}
	// .routr/ is untracked, so a brief's .routr/DECISIONS.md is not in the
	// worktree, and a report written there is lost with it.
	if c := wk.Checkout; c != nil {
		fmt.Fprintf(&b, " Your cwd is a git worktree of %s. Files git does not track, such as .routr/…, exist only in the main checkout:"+
			" read them from %s, and write reports to %s, not into the worktree.", c.Repo, c.Repo, p.WorkDir(wk.Chain))
	}
	if wk.Interactive {
		b.WriteString(" The user talks to you in this tab; finish only once they agree the work is done.")
	} else {
		// Claude ends its turn on a background job and waits to be told it
		// finished, which routr reads as maybe_done; this line keeps it
		// waiting within the turn instead.
		b.WriteString(" Do not end your turn while a command you started is still running, in the background or not:" +
			" routr takes an ended turn as finished. Wait for it within the turn.")
	}
	b.WriteString("\n\nWhen the work is finished and its checks have passed, run this command on its own, as your last command" +
		" (not after a ; or && in another command, and with no edits after it), then end your turn with a short final message:\n\n")
	verdict := ""
	if wk.WantVerdict {
		verdict = " --verdict pass|fail"
	}
	// Without the reason, half of the Haiku reviewers in a probe wrote the
	// summary in double quotes all the same.
	fmt.Fprintf(&b, "    %s --output %s%s 'SUMMARY'\n\nwith SUMMARY replaced by one line saying what you did or found. "+
		"Keep the single quotes: in double quotes the shell expands a $ or a backtick in it.\n", worker.DoneCommand(bin), out, verdict)
	if wk.WantVerdict {
		b.WriteString("\nUse --verdict pass if the work can go ahead as it is, fail if it must be revised first; say why in the output and your final message.\n")
	}
	if wk.Output == "" {
		b.WriteString("\nLeave out --output if you wrote no file.\n")
	}
	// Codex workers read the caller's routr skill to learn how to finish.
	b.WriteString("\nIf a check failed, say so in the output and the summary; do not report success. If the command fails," +
		" you have not finished: fix what it says and run it again. It is all of routr's protocol a worker needs:" +
		" you need no routr skill for it, and start no workers of your own unless your brief says to.\n")
	return b.String()
}
