package menu

import (
	"strings"
	"testing"
)

// Screens as rendered by herdr's pane read during the experiments.
const (
	claudeTrust = ` Quick safety check: Is this a project you created or one you trust?
 Claude Code'll be able to read, edit, and execute files here.
 Security guide
 ❯ No, exit
   Yes, I trust this folder
 Enter to confirm · Esc to cancel`

	claudeEdit = ` Do you want to make this edit to calc.py?
 ❯ 1. Yes
   2. Yes, and switch to accept edits (auto-approve file edits and common file commands) for this session (shift+tab)
   3. No
 Esc to cancel · Tab to amend`

	claudeAsk = `❯ 1. test_calc.py
     pytest-style default naming
  2. calc_test.py
     Suffix-style naming
  3. Type something.
─────────────────────────────────────────────────
  4. Chat about this
Enter to select · ↑/↓ to navigate · Esc to cancel`

	codexApproval = `  Would you like to run the following command?
  Reason: May I create test_calc.py?
  $ printf 'from calc import add' > test_calc.py
› 1. Yes, proceed (y)
  2. Yes, and don't ask again for commands that start with ` + "`printf 'from calc import add\\n\\nassert add(2, 3) ==" + `
     5\nassert add(0, -4) == -4\n' > test_calc.py` + "`" + ` (p)
  3. No, and tell Codex what to do differently (esc)`

	codexTrust = `  Trust this folder? Codex can read, edit, and run files here.
› 1. Trust and continue
  2. Quit
  enter continue · esc quit`

	// Claude just after a permission prompt was answered: herdr
	// still said blocked, and ● bullets plus the echoed prompt were read as
	// menus.
	claudeAfterAnswer = `❯ You are a worker agent that routr started for another agent. Do the task below in this directory. Keep your final
  message short: what you did, what you did not do, and where the results are.

  Write PLAN.md: a plan of at most 3 lines to fix the add bug in calc.py. Do not edit calc.py.

● Found the bug! The add function on line 2 returns a - b instead of a + b. Let me create the plan:

● Write(PLAN.md)
  ⎿  Wrote 3 lines to PLAN.md
      1 1. Change line 2 operator from ` + "`-` to `+`" + `
      2 2. Verify the fix with a simple test: ` + "`assert add(2, 3) == 5`" + `

  Submitting task completion to routr
  ⎿  $ "$ROUTR_BIN" done --output PLAN.md "Identified add function bug"

────────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────────
  ⏸ manual mode on · ? for shortcuts · ← for agents`
)

func TestParseAndAffirmative(t *testing.T) {
	tests := []struct {
		name        string
		screen      string
		wantOptions []string
		wantSel     int
		wantYes     int
		wantKeys    string
		numbered    bool
	}{
		{"claude trust defaults to No", claudeTrust,
			[]string{"No, exit", "Yes, I trust this folder"}, 0, 1, "down enter", false},
		{"claude permission", claudeEdit,
			[]string{"Yes", "Yes, and switch to accept edits (auto-approve file edits and common file commands) for this session (shift+tab)", "No"}, 0, 0, "enter", true},
		{"claude ask-user question", claudeAsk,
			[]string{"test_calc.py", "calc_test.py", "Type something."}, 0, -1, "", true},
		{"codex approval with wrapped option", codexApproval,
			[]string{"Yes, proceed (y)", "Yes, and don't ask again for commands that start with `printf 'from calc import add\\n\\nassert add(2, 3) ==", "No, and tell Codex what to do differently (esc)"}, 0, 0, "enter", true},
		{"codex trust", codexTrust, []string{"Trust and continue", "Quit"}, 0, 0, "enter", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, ok := Parse(tt.screen)
			if !ok {
				t.Fatal("no menu found")
			}
			if strings.Join(m.Options, "|") != strings.Join(tt.wantOptions, "|") {
				t.Errorf("options:\n got %q\nwant %q", m.Options, tt.wantOptions)
			}
			if m.Selected != tt.wantSel || m.Numbered != tt.numbered {
				t.Errorf("selected = %d, numbered = %v", m.Selected, m.Numbered)
			}
			yes := m.Affirmative()
			if yes != tt.wantYes {
				t.Fatalf("affirmative = %d, want %d", yes, tt.wantYes)
			}
			if yes >= 0 && strings.Join(m.Keys(yes), " ") != tt.wantKeys {
				t.Errorf("keys = %v, want %s", m.Keys(yes), tt.wantKeys)
			}
		})
	}
}

// Lines are the menu's own lines, the same wherever the highlight is.
func TestLinesLeaveOutTheMarker(t *testing.T) {
	moved := strings.Replace(strings.Replace(claudeAsk, "❯ 1.", "  1.", 1), "  2. calc", "❯ 2. calc", 1)
	a, _ := Parse(claudeAsk)
	b, _ := Parse(moved)
	want := []string{"1. test_calc.py", "   pytest-style default naming", "2. calc_test.py", "   Suffix-style naming", "3. Type something."}
	if strings.Join(a.Lines, "|") != strings.Join(want, "|") || strings.Join(b.Lines, "|") != strings.Join(want, "|") || a.Top != 0 || b.Selected != 1 {
		t.Errorf("lines:\n %q (top %d)\n %q (selected %d)\nwant %q", a.Lines, a.Top, b.Lines, b.Selected, want)
	}
	if m, _ := Parse(claudeEdit); m.Top != 1 || len(m.Lines) != 3 {
		t.Errorf("claude edit: top %d, lines %q", m.Top, m.Lines)
	}
}

func TestKeysMoveUpFromLowerSelection(t *testing.T) {
	m := Menu{Options: []string{"a", "b", "c"}, Selected: 2}
	if got := strings.Join(m.Keys(0), " "); got != "up up enter" {
		t.Errorf("keys = %s", got)
	}
}

func TestNoMenu(t *testing.T) {
	answered := claudeEdit + "\n\n● Update(calc.py)\n  ⎿  Updated calc.py\n\n● Running the tests\n  ⎿  1 passed\n● Done."
	// Option 1 scrolled away: answering 1 would pick the option shown as 2.
	scrolled := "  2. calc_test.py\n❯ 3. Type something."
	for _, s := range []string{"", "just some output\nno menu here", "❯ single prompt line", claudeAfterAnswer,
		strings.ReplaceAll(claudeAfterAnswer, "❯ \n", "❯\n"), answered, scrolled} {
		if _, ok := Parse(s); ok {
			t.Errorf("unexpected menu in %q", s)
		}
	}
}

// An option that saves a rule or reaches past the call it asks about is
// the user's to pick: the options as Claude Code 2.1.287 and Codex 0.160
// draw them (their bundles, and the screens above), cut where a narrow pane
// wraps them.
func TestLasting(t *testing.T) {
	for _, tt := range []struct {
		option string
		want   bool
	}{
		{"Yes", false},
		{"No", false},
		{"No, and tell Claude what to do differently (esc)", false},
		{"Yes, proceed (y)", false},
		{"Yes, just this once", false},
		{"Yes, but ask again next time", false},
		{"No, and ask again next time", false},
		{"Allow", false},
		{"Yes, I trust this folder", false},
		{"Trust and continue", false},
		{"test_calc.py", false},
		{"Yes, and don't ask again for: xargs -I {} sh -c 'echo \"═══ {} ═══\" && cat {}'", true},
		{"Yes, and don’t ask again for: curl", true}, // Claude draws some with a curly apostrophe
		{"Yes, and don't ask again for python3 test_calc.py commands in /tmp/work", true},
		{"Yes, and always allow access to /tmp/TestProbe/001 from this project", true},
		{"Yes, and always allow access to", true}, // wrapped
		{"Yes, and switch to accept edits (auto-approve file edits and common file commands) for this session (shift+tab)", true},
		{"Yes, and remember this directory", true},
		{"Yes, for this session", true},
		{"Yes, allow reading from /etc from this project", true},
		{"Yes, allow reading from /etc during this session", true},
		{"Yes, allow reading from", true}, // wrapped
		{"No, and block reads outside the working directories from now on", true},
		{"Yes, and don't ask again for commands that start with `printf 'from calc import add\\n\\nassert add(2, 3) ==", true},
		{"Yes, and allow this host in the future", true},
		{"No, and block this host in the future", true},
		{"Yes, grant these permissions for this turn", true},
		{"Allow for this session", true},
		{"Allow and don't ask me again", true},
	} {
		if got := Lasting(tt.option); got != tt.want {
			t.Errorf("Lasting(%q) = %v, want %v", tt.option, got, tt.want)
		}
	}
}
