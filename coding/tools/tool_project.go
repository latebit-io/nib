package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/latebit-io/nib/ai/llm"
)

// ProjectTaskAddTool appends one or more `[ ]` pending tasks to
// /project.md in a single call. The schema is batch-only: the LLM
// passes a `tasks` array (a single-element array for one task) so the
// common "lay out the whole plan in one round" case becomes one tool
// call instead of N. Each entry routes through the session's
// TaskTracker so the in-memory work tree stays in sync with demarkus.
//
// Activate and complete are handled by `update_task`; this tool only
// covers "add tasks that don't exist yet."
type ProjectTaskAddTool struct {
	tracker TaskMutator
}

// NewProjectTaskAddTool creates a project_task_add tool backed by a
// TaskMutator. Nil tracker disables the tool; the session-backed
// workspace implements TaskTracker (which composes TaskMutator) in
// normal runs.
func NewProjectTaskAddTool(tracker TaskMutator) *ProjectTaskAddTool {
	return &ProjectTaskAddTool{tracker: tracker}
}

// projectTaskAddEntry is one task to add. Each entry is independent —
// different entries can target different phase/feature pairs in the
// same call.
type projectTaskAddEntry struct {
	Phase   string `json:"phase"`
	Feature string `json:"feature"`
	Task    string `json:"task"`
	Link    string `json:"link"`
}

type projectTaskAddArgs struct {
	Tasks []projectTaskAddEntry `json:"tasks"`
}

// Definition returns the tool schema for the LLM.
func (t *ProjectTaskAddTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "project_task_add",
			Description: "Append one or more `[ ]` tasks to /project.md in a single call. Pass a `tasks` array; each entry has its own phase/feature/task and optional link. Phase matched by case-insensitive substring; feature created as `## Feature` if absent. Use a single batched call to lay out the project plan instead of one call per task.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"tasks": {
						Type:        "array",
						Description: "Tasks to append. Pass all tasks for a plan in one call.",
						Items: &llm.FunctionParam{
							Type: "object",
							Properties: map[string]llm.FunctionParam{
								"phase": {
									Type:        "string",
									Description: "Substring identifying the target phase (e.g. 'Phase 3' or 'Rendering').",
								},
								"feature": {
									Type:        "string",
									Description: "Feature heading title; created if absent.",
								},
								"task": {
									Type:        "string",
									Description: "Task title (imperative, what changes).",
								},
								"link": {
									Type:        "string",
									Description: "Optional memory doc path for supplementary context (e.g. '/game/renderer.md').",
								},
							},
							Required: []string{"phase", "feature", "task"},
						},
					},
				},
				Required: []string{"tasks"},
			},
		},
	}
}

// Execute appends each task in the batch via the TaskTracker. Partial
// success is the contract: an invalid entry (missing field, tracker
// error) does not stop the others. The tool result names what was
// added and what failed so the LLM can fix-and-retry only the failed
// entries instead of resending the whole batch.
//
// Validation order per entry mirrors the prior single-task tool so
// rejection shape stays familiar: phase required → feature required →
// task required → tracker call.
func (t *ProjectTaskAddTool) Execute(_ context.Context, call llm.ToolCall) ToolResult {
	var args projectTaskAddArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return textResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	if len(args.Tasks) == 0 {
		return textResult("Error: tasks array is required and must not be empty")
	}
	if t.tracker == nil {
		return textResult("Error: task tracking not available")
	}

	// Track first-occurrence index of each (phase, feature, task) triple
	// so a within-batch repeat surfaces as a "duplicate of tasks[N]"
	// failure rather than a second AddTask call. The LLM occasionally
	// emits the same task twice when sketching a plan; without this
	// guard the tracker would write two identical `[ ]` bullets to
	// /project.md and the second copy would orphan as soon as the
	// `update_task` flow matched the first by title.
	//
	// `link` is intentionally NOT part of the dedup key — it is
	// supplementary context, not part of the task identity.
	seen := make(map[string]int, len(args.Tasks))
	var added, failed []string
	for i, entry := range args.Tasks {
		phase := strings.TrimSpace(entry.Phase)
		feature := strings.TrimSpace(entry.Feature)
		task := strings.TrimSpace(entry.Task)
		link := strings.TrimSpace(entry.Link)

		if reason := validateTaskEntry(phase, feature, task); reason != "" {
			failed = append(failed, fmt.Sprintf("tasks[%d]: %s", i, reason))
			continue
		}

		// ASCII Unit Separator (0x1f) is the delimiter — guaranteed not
		// to appear in human-authored task titles, so the key is
		// unambiguous without escaping.
		key := phase + "\x1f" + feature + "\x1f" + task
		if firstIdx, dup := seen[key]; dup {
			failed = append(failed, fmt.Sprintf("tasks[%d] (%s > %s > %s): duplicate of tasks[%d]", i, phase, feature, task, firstIdx))
			continue
		}

		if err := t.tracker.AddTask(phase, feature, task, link); err != nil {
			// Do NOT register in `seen` on tracker failure — the first
			// occurrence never wrote a bullet, so a later identical entry
			// is a legitimate retry, not a duplicate. The documented
			// partial-success contract ("tracker errors don't stop the
			// others") means each tracker call is independent.
			failed = append(failed, fmt.Sprintf("tasks[%d] (%s > %s > %s): %v", i, phase, feature, task, err))
			continue
		}
		// Register only after the bullet has actually landed in
		// /project.md. From this point any later identical triple is a
		// genuine within-batch duplicate.
		seen[key] = i
		added = append(added, fmt.Sprintf("%s > %s > %s", phase, feature, task))
	}

	return textResult(formatTaskAddResult(added, failed))
}

// validateTaskEntry returns "" when the trimmed fields are all present
// and a short reason string when one is missing. Pulled out so the
// per-entry loop stays a single readable pass.
func validateTaskEntry(phase, feature, task string) string {
	switch {
	case phase == "":
		return "phase is required"
	case feature == "":
		return "feature is required"
	case task == "":
		return "task is required"
	}
	return ""
}

// formatTaskAddResult composes the tool result so the LLM sees one
// section per outcome category. Listing added entries verbatim
// confirms the round-trip and lets the LLM verify exact titles
// (which `update_task` will match against later); listing failures
// inline lets the LLM retry only the failing entries instead of
// resending the whole batch.
func formatTaskAddResult(added, failed []string) string {
	var b strings.Builder
	if len(added) > 0 {
		fmt.Fprintf(&b, "Added %d task(s):\n", len(added))
		for _, a := range added {
			fmt.Fprintf(&b, "  - %s\n", a)
		}
	}
	if len(failed) > 0 {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "Failed %d task(s):\n", len(failed))
		for _, f := range failed {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
	}
	if b.Len() == 0 {
		// Defensive: empty Tasks already returns an error above, so this
		// branch is unreachable today. Kept so a future refactor that
		// loosens the empty-array check fails closed instead of silently
		// returning "" to the LLM.
		return "Error: no tasks processed"
	}
	return strings.TrimRight(b.String(), "\n")
}
