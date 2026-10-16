package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bcherb2/herdr-routr/internal/herdr"
	"github.com/bcherb2/herdr-routr/internal/menu"
)

// Startup dialogs are recognized by their text as well as by status: herdr
// reports Claude's folder trust as blocked but Codex's as idle.
var (
	trustWords  = []string{"trust this folder", "do you trust", "trust the files", "trust the contents"}
	updateWords = []string{"update available", "new version"}
	skipWords   = []string{"skip", "not now", "later", "no thanks"}
)

// anyKeyNotice is Pi's startup notice (updates, changelog), which swallows
// the next key, and with it the start of a prompt.
const anyKeyNotice = "press any key to continue"

// Codex 0.159 asks about hooks it hasn't run before ("Hooks need review"),
// and reports idle under the question, so the brief would land in it. The
// worker continues without trusting them: that is the user's call. Its
// list of hooks (hooksList, behind "Review hooks") closes with esc.
const (
	hooksDialog = "hooks need review"
	hooksList   = "need review before they can run"
)

var untrustedWords = []string{"without trusting"}

// startupKeys reads the screen of a starting harness. dialog reports a
// startup menu (any menu while blocked, or a trust or update prompt), and
// keys what answers it: the approving option, or "skip" for an update. A
// dialog with nil keys is one routr leaves to the user. Keys without a dialog
// dismiss a notice.
func startupKeys(status herdr.AgentStatus, screen string) (keys []string, dialog bool) {
	lower := strings.ToLower(screen)
	if strings.Contains(lower, anyKeyNotice) || strings.Contains(lower, hooksList) {
		return []string{"esc"}, false
	}
	update, hooks := containsAny(lower, updateWords), strings.Contains(lower, hooksDialog)
	if status != herdr.Blocked && !update && !hooks && !containsAny(lower, trustWords) {
		return nil, false
	}
	m, ok := menu.Parse(screen)
	if !ok {
		return nil, false
	}
	if hooks {
		for i, o := range m.Options {
			if containsAny(strings.ToLower(o), untrustedWords) {
				return m.Keys(i), true
			}
		}
		return nil, true
	}
	if update {
		for i, o := range m.Options {
			if containsAny(strings.ToLower(o), skipWords) {
				return m.Keys(i), true
			}
		}
		return nil, true
	}
	if i := m.Affirmative(); i >= 0 {
		return m.Keys(i), true
	}
	return nil, true
}

// idleMenu reports a numbered menu at the bottom of the screen of a harness
// herdr calls idle: a question herdr has no rule for. Claude's Chrome
// permission menu ("Claude in Chrome wants to …", 1. Allow, 2. Deny) has no
// "Do you want to" line and no "Esc to cancel" footer, so herdr reported it
// idle and the worker went maybe_done with nobody asked. Only numbered
// menus count: Claude's and Codex's permission menus are numbered, and two
// lines typed in Claude's prompt box read as an unnumbered menu.
func idleMenu(screen string) bool {
	m, ok := menu.Parse(screen)
	return ok && m.Numbered
}

// question reads a dialog from the screen: its question (plus the detail
// under it, such as Codex's command) and numbered options, the options
// alone, and the menu's fingerprint. Without a menu (a free-text question)
// options is nil and fp "".
func question(screen string) (q string, options []string, fp string) {
	m, ok := menu.Parse(screen)
	if !ok {
		q, detail := dialogText(screen, "")
		return joinNonEmpty(q, detail), nil, ""
	}
	q, detail := dialogText(screen, m.Options[0])
	lines := []string{q, detail}
	for i, o := range m.Options {
		lines = append(lines, fmt.Sprintf("%d. %s", i+1, o))
	}
	return joinNonEmpty(lines...), m.Options, fingerprint(screen, m)
}

// fingerprint names the dialog of menu m on screen, so that `routr answer
// --expect` presses keys only on the question that was decided: a hash of
// its text from the top of the dialog (dialogTop) to the menu's last line,
// without whitespace, box edges, rules and the selection marker. The same
// dialog keeps its fingerprint when the highlight moves, a line outside it
// changes (Claude blinks the ● of the pending tool call above it; its footer
// changes with the highlight) or the pane is resized and its lines wrap
// anew. Another question, command or list of options gets another.
func fingerprint(screen string, m menu.Menu) string {
	above := strings.Split(screen, "\n")[:m.Top]
	var b strings.Builder
	for _, l := range slices.Concat(above[dialogTop(above):], m.Lines) {
		for _, r := range l {
			if !unicode.IsSpace(r) && !strings.ContainsRune(dialogEdges, r) {
				b.WriteRune(r)
			}
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// dialogEdges are drawn around and across a dialog, as wide as the pane.
const dialogEdges = "│┃╭╮╰╯─━═╌"

// dialogTop is the first line of the dialog above a menu, given the screen
// lines above its first option. Claude draws its dialogs as one block of
// lines under a rule across the pane (the command or diff between ╌ lines,
// then the question): the line after the rule. Otherwise the question, the
// paragraph of the last line with a '?' (Codex's, with the command under it),
// or without one the lines right above the menu. A rule further up, such as
// Codex's header box, is not the dialog's.
func dialogTop(above []string) int {
	end := len(above)
	for end > 0 && strings.TrimSpace(above[end-1]) == "" {
		end--
	}
	top := end
	for ; top > 0 && strings.TrimSpace(above[top-1]) != ""; top-- {
		if isRule(above[top-1]) {
			return top
		}
	}
	for q := end - 1; q >= 0 && !isRule(above[q]); q-- {
		if strings.Contains(above[q], "?") {
			for q > 0 && continues(above[q-1], above[q]) {
				q--
			}
			return q
		}
	}
	return top
}

// isRule reports a line drawn across the pane: a rule, or the top or bottom
// of a box.
func isRule(l string) bool {
	l = strings.Trim(strings.TrimSpace(l), "╭╮╰╯")
	return l != "" && strings.Trim(l, "─━═") == ""
}

// continues reports whether screen line next is prev wrapped (continuesLine,
// on the lines' text).
func continues(prev, next string) bool {
	text := func(l string) string { return strings.TrimSpace(strings.Trim(strings.TrimSpace(l), "│┃")) }
	prev, next = text(prev), text(next)
	return prev != "" && next != "" && continuesLine(prev, next)
}

func joinNonEmpty(lines ...string) string {
	var out []string
	for _, l := range lines {
		if l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// dialogText reads the dialog above its menu (or the whole screen when
// firstOption is ""). Lines wrapped by a narrow pane are re-joined first. The
// question runs up to the last '?'; the detail is what follows it up to the
// menu (e.g. Codex's command). Without a '?', the question is the last lines.
func dialogText(screen, firstOption string) (question, detail string) {
	lines := strings.Split(screen, "\n")
	end := len(lines)
	if firstOption != "" {
		for i := len(lines) - 1; i >= 0; i-- {
			if optionText(lines[i]) == firstOption {
				end = i
				break
			}
		}
	}
	var paras []string
	for _, l := range lines[:end] {
		l = strings.TrimSpace(strings.Trim(strings.TrimSpace(l), "│╭╮╰╯─┃"))
		if strings.Trim(l, "─━═ ") == "" || strings.Trim(l, ">❯› ") == "" { // rules, empty input prompts
			continue
		}
		if n := len(paras); n > 0 && continuesLine(paras[n-1], l) {
			paras[n-1] += " " + l
		} else {
			paras = append(paras, l)
		}
	}
	for i := len(paras) - 1; i >= 0; i-- {
		if k := strings.LastIndex(paras[i], "?"); k >= 0 {
			rest := append([]string{strings.TrimSpace(paras[i][k+1:])}, paras[i+1:]...)
			return paras[i][:k+1], clip(strings.TrimSpace(strings.Join(rest, " ")), 200)
		}
	}
	if len(paras) == 0 {
		return "(no question text on screen)", ""
	}
	return strings.Join(paras[max(0, len(paras)-3):], " "), ""
}

// optionText is a screen line as a menu option: without box edges, the
// selection marker and numbering.
func optionText(l string) string {
	l = strings.TrimSpace(strings.Trim(strings.TrimSpace(l), "│┃"))
	for _, mk := range []string{"❯", "›", "●", ">"} {
		l = strings.TrimSpace(strings.TrimPrefix(l, mk))
	}
	if n, rest, ok := strings.Cut(l, ". "); ok && n != "" && strings.Trim(n, "0123456789") == "" {
		l = rest
	}
	return strings.TrimSpace(l)
}

// continuesLine reports whether next is prev wrapped: prev ends mid-sentence
// and next starts in lower case.
func continuesLine(prev, next string) bool {
	r, _ := utf8.DecodeRuneInString(next)
	return unicode.IsLower(r) && !strings.ContainsAny(prev[len(prev)-1:], ".?!:")
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func containsAny(s string, subs []string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}
