package agent

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/latebit-io/junto/engine/llm"
)

// TaskTool handles task status updates in the project work tree.
// Uses the TaskTracker interface to update state through the session,
// avoiding direct memory writes and keeping the work tree consistent.
type TaskTool struct {
	tracker TaskTracker
}

// NewTaskTool creates a task tracking tool. The tracker is obtained via
// type assertion on the workspace — nil tracker disables the tool.
func NewTaskTool(tracker TaskTracker) *TaskTool {
	return &TaskTool{tracker: tracker}
}

// Definition returns the OpenAI-compatible function schema.
func (t *TaskTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name: "update_task",
			Description: "Update a task's status in the project plan. " +
				"Use 'activate' to mark a task as in-progress before starting work on it. " +
				"Use 'complete' to mark it done after finishing. " +
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
func (t *TaskTool) Execute(_ context.Context, call llm.ToolCall) ToolResult {
	var args taskArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return textResult("Error: invalid arguments: " + err.Error())
	}
	if args.Title == "" {
		return textResult("Error: title is required")
	}
	if t.tracker == nil {
		return textResult("Error: task tracking not available")
	}

	slog.Debug("update_task", "action", args.Action, "title", args.Title)

	var err error
	switch args.Action {
	case "activate":
		err = t.tracker.ActivateTask(args.Title)
	case "complete":
		err = t.tracker.CompleteTask(args.Title)
	default:
		return textResult("Error: action must be 'activate' or 'complete'")
	}

	if err != nil {
		slog.Warn("update_task failed", "action", args.Action, "title", args.Title, "err", err)
		return textResult("Error: " + err.Error())
	}

	slog.Debug("update_task succeeded", "action", args.Action, "title", args.Title)
	msg := "Task " + args.Action + "d: " + args.Title
	if args.Action == "complete" {
		return ToolResult{Content: msg, Effect: EffectTaskCompleted}
	}
	return textResult(msg)
}
