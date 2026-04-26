package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/latebit-io/junto/engine/llm"
)

// ProjectInitTool bootstraps the project's task-tracking document at
// /project.md so the LLM doesn't have to discover, work around, or
// hand-roll the work-tree document via [memory_publish]. Calling
// project_init on a fresh repo creates the skeleton and triggers a
// work-tree reload so the very next [project_task_add] or
// [update_task] succeeds — pre-tool, the agent had to publish via
// memory_publish, then guess that the in-session work-tree state
// wouldn't have refreshed, then either fail or fall back to creating
// a local .project.md (which the developer then deletes).
//
// Trust boundary: project_init never overwrites an existing
// /project.md. If the document already exists, the call reduces to a
// reload — protecting the developer's plan from accidental wipe by
// the LLM. Resetting the plan must be a deliberate developer action
// against demarkus.
//
// Distinct from memory_publish by design — patterns.md documents
// the split: memory_publish for session memory and arbitrary docs;
// project_init for the structured plan that drives task tracking
// and the autonomy gate.
type ProjectInitTool struct {
	tracker TaskTracker
}

// NewProjectInitTool creates a project_init tool backed by a
// TaskTracker. Nil tracker disables the tool; the session-backed
// workspace implements TaskTracker in normal runs.
func NewProjectInitTool(tracker TaskTracker) *ProjectInitTool {
	return &ProjectInitTool{tracker: tracker}
}

type projectInitArgs struct {
	Name   string   `json:"name"`
	Phases []string `json:"phases"`
}

// Definition returns the OpenAI-compatible tool schema.
func (t *ProjectInitTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name: "project_init",
			Description: "Bootstrap the project's task-tracking document at /project.md with the " +
				"given name and top-level phases. Call this BEFORE project_task_add or update_task " +
				"on a fresh repo — the work tree gate blocks task activation until /project.md " +
				"exists and is loaded. Idempotent: if /project.md already exists, the existing " +
				"plan is preserved and only the in-memory tree is refreshed. " +
				"Use project_init for the structured project plan; use memory_publish for session " +
				"notes, design docs, and other arbitrary content.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"name": {
						Type:        "string",
						Description: "Project display name (e.g. 'Pac-Man Clone'). Goes into the YAML frontmatter.",
					},
					"phases": {
						Type: "array",
						Description: "Top-level phase headings as plain titles (e.g. " +
							"['Foundation', 'Movement', 'Ghosts', 'Polish']). Each becomes an H1 in /project.md. " +
							"Pass them all in this call — project_init is idempotent and a second call will " +
							"NOT add phases to an existing /project.md. Empty array creates the doc with only " +
							"the project header; to add phases later, edit /project.md directly via memory tools.",
						Items: &llm.FunctionParam{Type: "string"},
					},
				},
				Required: []string{"name"},
			},
		},
	}
}

// Execute creates or refreshes /project.md and returns a summary the
// LLM can use to plan the next call (project_task_add, update_task).
func (t *ProjectInitTool) Execute(_ context.Context, call llm.ToolCall) ToolResult {
	var args projectInitArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return textResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		return textResult("Error: name is required")
	}
	if t.tracker == nil {
		return textResult("Error: task tracking not available")
	}

	if err := t.tracker.InitProject(name, args.Phases); err != nil {
		return textResult(fmt.Sprintf("Error: %v", err))
	}

	if len(args.Phases) == 0 {
		// project_task_add requires an existing phase, so no follow-up
		// task call works yet. Re-running project_init won't add phases
		// either (idempotent — see WorkTreeManager.InitProject). The
		// only forward path is editing /project.md directly via the
		// memory tools, then resuming task tracking.
		return textResult(fmt.Sprintf(
			"Initialised /project.md for %q (no phases). Add phase headings by editing "+
				"/project.md directly via memory tools (memory_publish/memory_append), then "+
				"call project_task_add to add tasks under them.", name))
	}
	return textResult(fmt.Sprintf(
		"Initialised /project.md for %q with phases: %s. Add tasks via project_task_add(phase, feature, task).",
		name, strings.Join(args.Phases, ", ")))
}
