package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/latebit-io/nib/ai/llm"
)

// TaskTool handles task status updates in the project work tree. Uses
// the TaskTracker interface — read-side (NextPendingTask) is needed so
// a successful complete can auto-activate the next pending task in the
// same call, sparing the LLM a round-trip turn on the transition. On a
// successful complete the tool calls the configured [TaskReviewer] so
// lint, smoke, and style findings land in the tool result alongside the
// auto-activation notice.
type TaskTool struct {
	tracker  TaskTracker
	reviewer TaskReviewer
}

// NewTaskTool creates a task tracking tool. The tracker is obtained via
// type assertion on the workspace — nil tracker disables the tool. The
// reviewer is invoked after a successful complete to run the
// post-completion pipeline; nil reviewer skips it.
func NewTaskTool(tracker TaskTracker, reviewer TaskReviewer) *TaskTool {
	return &TaskTool{tracker: tracker, reviewer: reviewer}
}

// Definition returns the OpenAI-compatible function schema.
func (t *TaskTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name: "update_task",
			Description: "Update a task's status in the project plan. " +
				"Prefer bundling the transition into the mutating tool call via `activate_task` / `complete_task` when the lifecycle coincides with an edit; use this standalone call when no mutating tool accompanies the transition (e.g., a review-only turn). " +
				"'activate' marks a task in-progress before work begins. " +
				"'complete' marks it done after finishing — on success the next pending task is auto-activated and named in the result, so you can proceed straight to its work without a separate activate call. " +
				"The title must exactly match a task item from the project plan.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"title": {
						Type:        "string",
						Description: "Exact task title from the project plan (must match a - [ ], - [>], or - [x] item)",
					},
					"action": {
						Type:        "string",
						Description: "Either 'activate' (mark as in-progress [>]) or 'complete' (mark as done [x]).",
					},
				},
				Required: []string{"title", "action"},
			},
		},
	}
}

// taskArgs holds the parsed arguments for the update_task tool.
type taskArgs struct {
	Title  string `json:"title"`
	Action string `json:"action"`
}

// Execute handles the tool call.
func (t *TaskTool) Execute(ctx context.Context, call llm.ToolCall) ToolResult {
	if len(call.Function.Arguments) > maxToolArgsBytes {
		return errorResult("Error: arguments too large")
	}
	var args taskArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return errorResult("Error: invalid arguments: " + err.Error())
	}
	if args.Title == "" {
		return errorResult("Error: title is required")
	}
	if t.tracker == nil {
		return errorResult("Error: task tracking not available")
	}

	slog.Debug("update_task", "action", args.Action, "title", args.Title)

	var err error
	switch args.Action {
	case "activate":
		err = t.tracker.ActivateTask(args.Title)
	case "complete":
		err = t.tracker.CompleteTask(args.Title)
	default:
		return errorResult("Error: action must be 'activate' or 'complete'")
	}

	if err != nil {
		slog.Warn("update_task failed", "action", args.Action, "title", args.Title, "err", err)
		return errorResult("Error: " + err.Error())
	}

	slog.Debug("update_task succeeded", "action", args.Action, "title", args.Title)
	msg := "Task " + args.Action + "d: " + args.Title
	if args.Action != "complete" {
		return textResult(msg)
	}
	if t.reviewer != nil {
		msg = t.reviewer.OnComplete(ctx, msg)
	}
	return textResult(msg + AutoActivateNext(t.tracker))
}

// AutoActivateNext finds the next pending task, activates it, and
// returns the suffix to append to a complete-result message. Shared by
// update_task(complete) and the agent's lifecycle complete_task bundle
// so both paths report identically. Best-effort: activation failure is
// surfaced inline for the LLM to retry; the complete already persisted.
func AutoActivateNext(tracker TaskTracker) string {
	next := tracker.NextPendingTask()
	if next == "" {
		return "\n\nAll tasks complete."
	}
	if err := tracker.ActivateTask(next); err != nil {
		slog.Warn("auto-activate next task failed", "title", next, "err", err)
		return fmt.Sprintf("\n\nNext pending task: %q (auto-activate failed: %v — call update_task with action=\"activate\" to retry).", next, err)
	}
	slog.Debug("auto-activated next task", "title", next)
	return fmt.Sprintf("\n\nNext task auto-activated: %s", next)
}
