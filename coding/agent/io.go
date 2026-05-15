package agent

import (
	"context"
	"strings"

	"github.com/latebit-io/nib/ai/llm"
)

// Wrapper-side I/O helpers used by the foundation hook wiring.
//
// flushDirtyBuffers coordinates with the frontend to autosave open
// buffers before each tool dispatch — by invoking the frontend-supplied
// [FlushDirtyBuffersFunc] callback registered at construction.
// planningToolDefs filters the advertised tool slice to the planning-
// mode subset. Both are wrapper-private utilities the foundation hooks
// call into.

// planningToolDefs returns the agent's tool definitions with the
// planning-mode blocklist applied. The full set lives on
// [Agent.toolDefs]; this filter is consulted on every Stream so a
// runtime mode flip takes effect on the next turn.
func (a *Agent) planningToolDefs() []llm.ToolDef {
	defs := make([]llm.ToolDef, 0, len(a.toolDefs))
	for _, def := range a.toolDefs {
		name := strings.ToLower(def.Function.Name)
		if a.planningBlocklist[name] {
			continue
		}
		defs = append(defs, def)
	}
	return defs
}

// flushDirtyBuffers asks the frontend to save all dirty buffers to
// disk via the [FlushDirtyBuffersFunc] callback registered on
// [NewOptions], then invalidates the corresponding [FileCache] entries
// for every file the callback reports as saved. Called from the
// foundation BeforeToolCall hook before every tool dispatch — not once
// per batch — because an earlier tool (e.g. edit_file) may modify
// buffers a later tool needs on disk.
//
// When no callback is registered the step is a no-op: a frontend
// without in-memory buffers (the headless case) has no flush to
// perform. Cache invalidation happens for every successfully-saved
// path even if the callback returned a partial error, matching the
// pre-callback FlushBuffers-event behavior.
func (a *Agent) flushDirtyBuffers(ctx context.Context) error {
	if a.flushDirtyBuffersFn == nil {
		return nil
	}
	saved, err := a.flushDirtyBuffersFn(ctx)
	for _, p := range saved {
		a.cache.Invalidate(p)
	}
	return err
}
