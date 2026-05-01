package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/engine/event"
)

// Wrapper-side I/O helpers used by the foundation hook wiring.
//
// flushDirtyBuffers coordinates with the frontend to autosave open
// buffers before each tool dispatch — bypassing the agent goroutine's
// no-touch policy on TUI buffer state by routing the I/O through a
// FlushBuffers event with a result channel. planningToolDefs filters
// the advertised tool slice to the planning-mode subset.
//
// Both used to live in the inline turn pipeline; after the 8c cutover
// they are wrapper-private utilities the foundation hooks call into.

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
// disk, then invalidates the corresponding cache entries. The actual
// I/O runs on the frontend's goroutine (via FlushBuffers event) so
// the agent goroutine never touches TUI-owned buffer state. Called
// from the foundation BeforeToolCall hook before every tool dispatch
// — not once per batch — because an earlier tool (e.g. edit_file)
// may modify buffers a later tool needs on disk.
//
// Bounded timeouts on both the enqueue and the response so a frontend
// that stops draining surfaces visibly rather than wedging the agent.
func (a *Agent) flushDirtyBuffers(ctx context.Context) error {
	resultCh := make(chan event.FlushResult, 1)

	enqueueTimeout := time.NewTimer(5 * time.Second)
	defer enqueueTimeout.Stop()
	select {
	case a.events <- event.FlushBuffers{Result: resultCh}:
	case <-ctx.Done():
		return ctx.Err()
	case <-enqueueTimeout.C:
		return fmt.Errorf("autosave: event queue not draining")
	}

	responseTimeout := time.NewTimer(5 * time.Second)
	defer responseTimeout.Stop()
	select {
	case res := <-resultCh:
		for _, p := range res.Saved {
			a.cache.Invalidate(p)
		}
		return res.Err
	case <-ctx.Done():
		return ctx.Err()
	case <-responseTimeout.C:
		return fmt.Errorf("autosave: frontend response timed out")
	}
}
