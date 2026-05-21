package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/llm"
)

// lifecycleActivateDescription is the schema-side prose handed to the
// LLM for the activate_task field on mutating tool schemas. Kept as a
// package constant so the prompt template can reference the same
// wording without copy-paste drift.
const lifecycleActivateDescription = "Optional. Title of a task to activate before running this tool. " +
	"Use this when you're starting work on a new task — saves a separate update_task(activate) call. " +
	"Title must match exactly a `- [ ]` task in /project.md."

// lifecycleCompleteDescription is the schema-side prose for the
// complete_task field. Same rationale as [lifecycleActivateDescription].
const lifecycleCompleteDescription = "Optional. When true, auto-completes the currently active task after this tool succeeds. " +
	"Saves a separate update_task(complete) call when this tool call is the final action of the task. " +
	"The next pending task auto-activates (named in the result)."

// lifecycleAwareTool wraps a mutating tool to advertise the optional
// activate_task / complete_task fields in its JSON schema. Execute is
// pass-through — the lifecycle dispatch happens entirely in the
// Before/AfterToolCall hooks ([Agent.runLifecycleActivate] and
// [Agent.runLifecycleComplete]). This decorator is the schema-side
// counterpart that tells the LLM the fields are accepted.
//
// Wrapping happens once at toolset assembly ([Agent.registerTools]);
// internal tool packages stay clean — they don't know they're being
// wrapped.
type lifecycleAwareTool struct {
	Tool
}

// Definition returns the wrapped tool's definition with the
// activate_task / complete_task fields injected into Properties. The
// fields are NOT added to Required so the bundle is strictly additive
// — every existing tool call shape continues to validate.
func (t lifecycleAwareTool) Definition() llm.ToolDef {
	return augmentWithLifecycleFields(t.Tool.Definition())
}

// augmentWithLifecycleFields returns a copy of def with the optional
// activate_task / complete_task fields added to Properties. Idempotent
// — a definition that already advertises both fields is returned
// unchanged. The Properties map is copied, not mutated in place, so
// the wrapped tool's cached Definition() is unaffected.
func augmentWithLifecycleFields(def llm.ToolDef) llm.ToolDef {
	if _, hasActivate := def.Function.Parameters.Properties["activate_task"]; hasActivate {
		if _, hasComplete := def.Function.Parameters.Properties["complete_task"]; hasComplete {
			return def
		}
	}
	props := make(map[string]llm.FunctionParam, len(def.Function.Parameters.Properties)+2)
	for k, v := range def.Function.Parameters.Properties {
		props[k] = v
	}
	props["activate_task"] = llm.FunctionParam{
		Type:        "string",
		Description: lifecycleActivateDescription,
	}
	props["complete_task"] = llm.FunctionParam{
		Type:        "boolean",
		Description: lifecycleCompleteDescription,
	}
	out := def
	out.Function.Parameters.Properties = props
	return out
}

// Task-lifecycle bundle for mutating tool calls.
//
// Mutating tool calls (those listed in [mutatingTools]) may carry two
// optional fields — activate_task (string) and complete_task (bool) —
// that fold the project-task lifecycle into the same turn as the
// mutation. The fields are advertised in the tool schemas by Commit 2
// of /nib/plans/update-task-bundling.md; this file owns the parse +
// run logic on the dispatch side.
//
// The work-tree dispatch gate ([Agent.enforceActiveTaskGate]) is
// unchanged: an absent active task still refuses mutating dispatch.
// The bundle's activate fires BEFORE the gate so a successful
// activation makes the gate pass naturally; an empty bundle is a
// no-op and the gate behaves exactly as it did before.

// lifecycleBundle is the parsed activate_task / complete_task pair from
// a tool call's raw JSON arguments. The zero value carries no fields
// and is treated as "no behavior change."
type lifecycleBundle struct {
	ActivateTask string `json:"activate_task"`
	CompleteTask bool   `json:"complete_task"`
}

// parseLifecycleBundle extracts the optional fields from the raw JSON
// tool arguments. Returns the zero-value bundle on any unmarshal error
// or when neither field is present — callers treat that as a no-op.
//
// The tools' own JSON unmarshal ignores extra fields (none of the
// mutating tools use DisallowUnknownFields), so the args string is
// forwarded to Execute verbatim — no stripping is required.
func parseLifecycleBundle(rawArgs string) lifecycleBundle {
	var b lifecycleBundle
	if rawArgs == "" {
		return b
	}
	if err := json.Unmarshal([]byte(rawArgs), &b); err != nil {
		slog.Debug("lifecycle: ignoring malformed tool args", "err", err)
	}
	return b
}

// runLifecycleActivate handles the activate half of the bundle in the
// BeforeToolCall path. Runs after the planning blocklist and before
// the active-task gate so a successful activation leaves the gate
// pointing at the just-activated task.
//
// Carve-outs mirror [Agent.enforceActiveTaskGate]: workspaces that do
// not expose [TaskTracker] silently skip the bundle, matching the
// gate's nil-tracker case.
//
// A failed activate (unknown title, persistence error) aborts the
// tool dispatch with the activate error surfaced as the block reason
// — the tool's Execute does NOT run.
func (a *Agent) runLifecycleActivate(_ context.Context, b lifecycleBundle, toolName string) upagent.BeforeToolCallResult {
	if b.ActivateTask == "" {
		return upagent.BeforeToolCallResult{}
	}
	tt, ok := a.workspace.(TaskTracker)
	if !ok {
		return upagent.BeforeToolCallResult{}
	}
	if err := tt.ActivateTask(b.ActivateTask); err != nil {
		slog.Warn("lifecycle: activate_task failed", "tool", toolName, "title", b.ActivateTask, "err", err)
		return upagent.BeforeToolCallResult{
			Block:  true,
			Reason: fmt.Sprintf("Error: activate_task %q failed: %v", b.ActivateTask, err),
		}
	}
	slog.Debug("lifecycle: activate_task fired", "tool", toolName, "title", b.ActivateTask)
	return upagent.BeforeToolCallResult{}
}

// runLifecycleComplete handles the complete half of the bundle in the
// AfterToolCall path. Returns a non-nil *string content override when
// the tool succeeded and complete_task=true was requested — the
// override is the tool's original content with the auto-complete
// trailer appended:
//
//   - Success: appends "Task completed: <title>" + review findings +
//     "Next task auto-activated: <next>" (or "All tasks complete.").
//   - Complete failure: appends "[auto-complete failed: <reason>]"
//     and preserves the tool's success result so the LLM can retry
//     the complete via standalone update_task.
//
// Skipped (returns nil) when the tool errored, complete_task was
// unset, or no TaskTracker is available — every other path is a
// no-op so the wrapper is strictly additive.
func (a *Agent) runLifecycleComplete(ctx context.Context, b lifecycleBundle, result upagent.ToolResult) *string {
	if !b.CompleteTask {
		return nil
	}
	if result.IsError {
		slog.Debug("lifecycle: complete_task skipped (tool errored)")
		return nil
	}
	tt, ok := a.workspace.(TaskTracker)
	if !ok {
		return nil
	}
	title := activeTaskTitle(tt.ActiveTaskPath())
	if title == "" {
		merged := result.Content + "\n[auto-complete skipped: no active task to complete]"
		return &merged
	}
	if err := tt.CompleteTask(title); err != nil {
		slog.Warn("lifecycle: complete_task failed", "title", title, "err", err)
		merged := fmt.Sprintf("%s\n[auto-complete failed: %v]", result.Content, err)
		return &merged
	}
	trailer := a.lifecycleCompleteTrailer(ctx, tt, title)
	merged := result.Content + trailer
	return &merged
}

// lifecycleCompleteTrailer formats the trailer text appended to a
// successful bundled tool result after the active task has been
// marked done. Mirrors the shape of tool_task.go's
// update_task(complete) response — runs the [tools.TaskReviewer]
// pipeline ([Agent.OnComplete]: lint, smoke, style findings) over
// the "Task completed: <title>" base, then appends the
// auto-activate-next suffix from PR #169.
func (a *Agent) lifecycleCompleteTrailer(ctx context.Context, tt TaskTracker, completedTitle string) string {
	base := "\nTask completed: " + completedTitle
	base = a.OnComplete(ctx, base)
	base += autoActivateNextTrailer(tt)
	return base
}

// autoActivateNextTrailer mirrors [tools.TaskTool] autoActivateNext.
// Extracted as a free function so the lifecycle bundle can share the
// same "complete → auto-activate next" semantics as standalone
// update_task(complete) calls. Failures are surfaced inline so the
// LLM can retry the activate manually; the complete itself is never
// rolled back (already persisted by [TaskTracker.CompleteTask]).
func autoActivateNextTrailer(tt TaskTracker) string {
	next := tt.NextPendingTask()
	if next == "" {
		return "\n\nAll tasks complete."
	}
	if err := tt.ActivateTask(next); err != nil {
		slog.Warn("lifecycle: auto-activate-next failed", "title", next, "err", err)
		return fmt.Sprintf("\n\nNext pending task: %q (auto-activate failed: %v — call update_task with action=\"activate\" to retry).", next, err)
	}
	return "\n\nNext task auto-activated: " + next
}

// activeTaskTitle extracts the leaf title from a " > "-joined
// ancestry path (the format [WorkTreeManager.activeGoalLocked]
// produces). The bundled-form complete reads the title from the
// tracker rather than asking the LLM to repeat what it just
// activated — matches the plan's "the user is implicitly asking to
// complete the task currently active" contract.
func activeTaskTitle(activePath string) string {
	if activePath == "" {
		return ""
	}
	if i := strings.LastIndex(activePath, " > "); i >= 0 {
		return activePath[i+len(" > "):]
	}
	return activePath
}
