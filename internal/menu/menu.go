// Package menu reads selection menus from a harness's rendered screen and
// computes the keys that pick an option.
//
// Harness dialogs differ in their defaults: Claude's folder-trust dialog
// highlights "No, exit", so pressing Enter to approve quits Claude. Any
// automated or relayed answer must pick an option explicitly.
package menu

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Menu is a vertical list of options with one highlighted.
type Menu struct {
	Options  []string // option text, numbering stripped
	Selected int      // index of the highlighted option
	Numbered bool     // the options were numbered ("1. Yes")
	// Lines are the menu's screen lines, its options and the lines under
	// them (descriptions, wrapped text), without box edges and the
	// selection marker: the same whichever option is highlighted. Top is
	// the screen line of the first.
	Lines []string
	Top   int
}

// Selection markers used by Claude Code (❯) and Codex (›). Gemini's ● is
// left out: Claude draws ● before every message and tool call, so part of
// a Claude transcript would read as a menu.
var markers = []string{"❯ ", "› ", "> "}

// maxTrailing is how many lines (not counting blanks and rules) may follow
// a menu. A live dialog sits at the bottom of the screen, with at most a
// footer and a status line under it; a menu higher up is history, such as
// a prompt echoed in the transcript.
const maxTrailing = 3

var numbering = regexp.MustCompile(`^\d+\.\s+`)

// Parse finds the menu around the highlighted line, if any. Options are the
// lines whose text starts in the same column as the highlighted option's
// text (and, when options are numbered, that are numbered: Codex prints its
// explanation in the same column); deeper-indented lines are descriptions or
// wrapped text, and anything else ends the menu, which must be near the
// bottom of the screen (maxTrailing). A numbered menu starts at 1, so that
// option N is the one shown as N.
func Parse(screen string) (Menu, bool) {
	lines := strings.Split(screen, "\n")
	for i := range lines {
		lines[i] = stripBox(lines[i])
	}
	sel, col := -1, 0
	for i := len(lines) - 1; i >= 0; i-- { // the active dialog is at the bottom
		if c, ok := markedTextCol(lines[i]); ok {
			sel, col = i, c
			break
		}
	}
	if sel < 0 {
		return Menu{}, false
	}
	numbered := numbering.MatchString(runeSlice(lines[sel], col))
	start := sel // the first option: up past options and the descriptions under them
	for i := sel - 1; i >= 0; i-- {
		if isOption(lines[i], col, numbered) {
			start = i
		} else if c, ok := textCol(lines[i]); !ok || c <= col {
			break
		}
	}
	if numbered && !strings.HasPrefix(runeSlice(lines[start], col), "1.") {
		return Menu{}, false
	}
	m := Menu{Numbered: numbered, Top: start}
	end := len(lines)
scan:
	for i := start; i < len(lines); i++ {
		l := lines[i]
		if i == sel {
			m.Selected = len(m.Options)
			m.Options = append(m.Options, clean(runeSlice(l, col)))
			continue
		}
		c, ok := textCol(l)
		switch {
		case isOption(l, col, numbered):
			m.Options = append(m.Options, clean(runeSlice(l, col)))
		case !ok || c <= col:
			end = i
			break scan
		}
	}
	for _, l := range lines[start:end] {
		m.Lines = append(m.Lines, runeSlice(l, col))
	}
	return m, len(m.Options) > 1 && trailing(lines[end:]) <= maxTrailing
}

// trailing counts the lines that are neither blank nor a rule.
func trailing(lines []string) int {
	n := 0
	for _, l := range lines {
		if strings.Trim(l, " ─━═╌") != "" {
			n++
		}
	}
	return n
}

// Affirmative returns the first option that approves or trusts, or -1.
func (m Menu) Affirmative() int {
	for i, o := range m.Options {
		l := strings.ToLower(o)
		if strings.HasPrefix(l, "no") || strings.HasPrefix(l, "don't") || strings.HasPrefix(l, "do not") {
			continue
		}
		for _, w := range []string{"yes", "trust", "allow", "proceed", "continue", "approve"} {
			if strings.HasPrefix(l, w) {
				return i
			}
		}
	}
	return -1
}

// lasting are what an option says when it decides more than the one call
// its question is about, as Claude Code 2.1.287 and Codex 0.160 word them.
// Claude saves a rule into the repo's .claude/settings.local.json ("Yes,
// and don't ask again for … commands in /path", "Yes, and always allow
// access to /dir from this project", "Yes, allow reading from /dir from
// this project", "Yes, and remember this directory") or widens the session
// ("Yes, and switch to accept edits … for this session"). Codex saves an
// exec-policy rule ("Yes, and don't ask again for commands that start with
// …"), or allows a host, files or permissions for the session, the turn or
// for good ("Yes, grant these permissions for this turn", "Allow for this
// session"). Every "Yes, and …" of either is one of these; a "No, and block
// …" lasts too. Only the first line of a wrapped option is on the menu, so
// the words that tell come first in each.
var lasting = []string{"yes, and", "no, and block", "yes, allow reading from", "yes, grant", "don't ask", "always",
	"remember", "keep allowing", "from now on", "in the future", "this project", "this session", "this conversation", "this turn"}

// Lasting reports whether option decides more than the call its question
// is about: it saves a rule or allows (or blocks) something for the rest of
// the session. Only the user may pick one; an answer by the main agent or
// by Jev may not (skills/routr/cli.md, "Answering a question").
func Lasting(option string) bool {
	l := strings.ToLower(strings.ReplaceAll(option, "’", "'"))
	for _, s := range lasting {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

// Keys returns the key presses that move the highlight to option i and
// confirm it.
func (m Menu) Keys(i int) []string {
	var keys []string
	for d := i - m.Selected; d > 0; d-- {
		keys = append(keys, "down")
	}
	for d := m.Selected - i; d > 0; d-- {
		keys = append(keys, "up")
	}
	return append(keys, "enter")
}

func stripBox(l string) string {
	t := strings.TrimSuffix(strings.TrimRight(l, " "), "│")
	if i := strings.Index(t, "│"); i >= 0 && strings.TrimSpace(t[:i]) == "" {
		t = strings.Repeat(" ", utf8.RuneCountInString(t[:i])+1) + t[i+len("│"):]
	}
	return t
}

// markedTextCol reports the rune column where a highlighted option's text starts.
func markedTextCol(l string) (int, bool) {
	c, ok := textCol(l)
	if !ok {
		return 0, false
	}
	rest := runeSlice(l, c)
	for _, mk := range markers {
		if strings.HasPrefix(rest, mk) {
			return c + utf8.RuneCountInString(mk), true
		}
	}
	return 0, false
}

func isOption(l string, col int, numbered bool) bool {
	c, ok := textCol(l)
	return ok && c == col && (!numbered || numbering.MatchString(runeSlice(l, col)))
}

// textCol is the rune column of the first non-space character.
func textCol(l string) (int, bool) {
	for i, r := range []rune(l) {
		if r != ' ' {
			return i, true
		}
	}
	return 0, false
}

func runeSlice(l string, from int) string {
	r := []rune(l)
	if from >= len(r) {
		return ""
	}
	return string(r[from:])
}

func clean(s string) string {
	return strings.TrimSpace(numbering.ReplaceAllString(strings.TrimSpace(s), ""))
}
