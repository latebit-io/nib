package tools

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/latebit-io/nib/ai/llm"
)

// TaskTool handles task status updates in the project work tree.
// Uses the TaskMutator interface to update state through the session,
// avoiding direct memory writes and keeping the work tree consistent.
// On a successful "complete" action the tool calls the configured
// [TaskReviewer] so the LLM sees lint, smoke, and style review
// feedback inline with the tool result.
type TaskTool struct {
	tracker  TaskMutator
	reviewer TaskReviewer
}

// NewTaskTool creates a task tracking tool. The tracker is obtained via
// type assertion on the workspace — nil tracker disables the tool.
// TaskMutator covers ActivateTask and CompleteTask; the read-side
// surface is unused here so depending on it would be overreach. The
// reviewer is invoked after a successful complete to run the
// post-completion pipeline; nil reviewer skips it.
func NewTaskTool(tracker TaskMutator, reviewer TaskReviewer) *TaskTool {
	return &TaskTool{tracker: tracker, reviewer: reviewer}
}

// Definition returns the OpenAI-compatible function schema.
func (t *TaskTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "update_task",
			Description: "Set a project-plan task's status: 'activate' (start) or 'complete' (done). Title must match an existing task verbatim.",
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
	if args.Action == "complete" && t.reviewer != nil {
		return textResult(t.reviewer.OnComplete(ctx, msg))
	}
	return textResult(msg)
}
