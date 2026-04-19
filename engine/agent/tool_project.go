package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/latebit-io/junto/engine/llm"
)

// ProjectTaskAddTool appends a new `[ ]` pending task to /project.md under
// the specified phase and feature. The feature is created if absent. All
// mutations route through the session's TaskTracker so the in-memory work
// tree (and the top bar derived from it) stay in sync with demarkus.
//
// Activate and complete are handled by the existing `update_task` tool —
// this tool exists solely to cover "add a task that doesn't exist yet."
type ProjectTaskAddTool struct {
	tracker TaskTracker
}

// NewProjectTaskAddTool creates a project_task_add tool backed by a
// TaskTracker. Nil tracker disables the tool; the session-backed
// workspace implements TaskTracker in normal runs.
func NewProjectTaskAddTool(tracker TaskTracker) *ProjectTaskAddTool {
	return &ProjectTaskAddTool{tracker: tracker}
}

type projectTaskAddArgs struct {
	Phase   string `json:"phase"`
	Feature string `json:"feature"`
	Task    string `json:"task"`
	Link    string `json:"link"`
}

// Definition returns the tool schema for the LLM.
func (t *ProjectTaskAddTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name: "project_task_add",
			Description: "Append a new `[ ]` pending task to /project.md under the given phase and " +
				"feature. The phase is located by case-insensitive substring match (e.g. 'Phase 3' " +
				"or part of its title). The feature is matched under that phase; if not present, " +
				"it is created as a new `## Feature` heading. An optional `link` argument attaches " +
				"a supplementary memory-doc path, appended to the task title as a markdown link " +
				"(e.g. '- [ ] Make renderer ([details](/game/renderer.md))').",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"phase": {
						Type:        "string",
						Description: "Substring identifying the target phase (e.g. 'Phase 3' or 'Rendering')",
					},
					"feature": {
						Type:        "string",
						Description: "Feature heading title; created if absent",
					},
					"task": {
						Type:        "string",
						Description: "Task title (imperative, what changes)",
					},
					"link": {
						Type:        "string",
						Description: "Optional memory doc path for supplementary context (e.g. '/game/renderer.md')",
					},
				},
				Required: []string{"phase", "feature", "task"},
			},
		},
	}
}

// Execute appends the new task via the TaskTracker.
func (t *ProjectTaskAddTool) Execute(_ context.Context, call llm.ToolCall) ToolResult {
	var args projectTaskAddArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return textResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	if strings.TrimSpace(args.Phase) == "" {
		return textResult("Error: phase is required")
	}
	if strings.TrimSpace(args.Feature) == "" {
		return textResult("Error: feature is required")
	}
	if strings.TrimSpace(args.Task) == "" {
		return textResult("Error: task is required")
	}
	if t.tracker == nil {
		return textResult("Error: task tracking not available")
	}

	if err := t.tracker.AddTask(args.Phase, args.Feature, args.Task, args.Link); err != nil {
		return textResult(fmt.Sprintf("Error: %v", err))
	}

	return textResult(fmt.Sprintf(
		"Added: %s > %s > %s",
		strings.TrimSpace(args.Phase),
		strings.TrimSpace(args.Feature),
		strings.TrimSpace(args.Task),
	))
}
