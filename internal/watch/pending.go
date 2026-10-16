package watch

import (
	"fmt"
	"time"

	"github.com/bcherb2/herdr-routr/internal/hook"
	"github.com/bcherb2/herdr-routr/internal/registry"
	"github.com/bcherb2/herdr-routr/internal/transcript"
	"github.com/bcherb2/herdr-routr/internal/worker"
)

// pendingInputMax bounds a pending call's input in the registry: a command
// or an Edit fits whole, a long Write's content is cut.
const pendingInputMax = 4 << 10

// pending is the tool call the question wk blocked on is about, or nil if
// it is about none. Claude's PermissionRequest hook names the call a prompt
// is for (its subagents' too, by Claude's docs); its record counts when it
// is newer than the prompt that began the turn and than any result the
// transcript has recorded since (else it was answered). It counts also
// when the transcript has no call without a result: the hook runs before
// Claude draws the prompt, and Claude may write the call only later (an
// rm -rf can ask with no assistant record in the transcript yet).
// Otherwise, and for Codex and Pi, the transcript's oldest call without a
// result. Neither means the question is not about one (a menu of the
// harness's own).
func (w *Watcher) pending(wk registry.Worker, st worker.Status) *registry.ToolCall {
	var calls []transcript.Message
	var settled time.Time
	if parse, err := transcript.ForKind(wk.Harness); err == nil {
		if file := first(st.Transcript, wk.Transcript); file != "" {
			if calls, settled, err = transcript.Unresolved(file, parse); err != nil {
				w.Log.Printf("%s: reading its transcript for the call it asks about: %v", wk.Label(), err)
			}
		}
	}
	rec, ok, err := hook.Latest(w.Paths.HookLog(wk.ID), hook.PermissionRequest)
	if err != nil {
		w.Log.Printf("%s: reading its hook log: %v", wk.Label(), err)
	}
	if at := rec.Time(); ok && at.After(settled) && at.After(wk.Prompted) {
		if p, ok := hook.ParsePermission(rec.Payload); ok {
			return toolCall(p.Tool, string(p.Input))
		}
	}
	if len(calls) == 0 {
		return nil
	}
	return toolCall(calls[0].Tool, calls[0].Text)
}

// toolCall is a pending call as the registry keeps it, its input cut to
// pendingInputMax bytes.
func toolCall(tool, input string) *registry.ToolCall {
	c := &registry.ToolCall{Tool: tool, Summary: transcript.Summary(tool, input), Input: input}
	if len(input) > pendingInputMax {
		cut := cutEnd(input, pendingInputMax)
		c.Input = cut + fmt.Sprintf("… [routr: %d bytes cut]", len(input)-len(cut))
	}
	return c
}
