package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/latebit-io/nib/ai/llm"
)

// ProjectPhaseAddTool appends one or more top-level phase (h1)
// headings to an existing /project.md. It closes the work-tree gap
// left by project_init (idempotent — seeds phases on a fresh repo but
// refuses to touch an existing plan) and project_task_add (creates
// features and tasks under an *existing* phase, never a new phase). A
// bare descriptive title is auto-numbered "Phase N: Title" so the
// strict schema holds; a title already in that form is used verbatim.
// Phases append at the end.
//
// Like project_task_add, the schema is batch-only: pass a `phases`
// array so laying out several phases is one tool call. Each entry
// routes through the session's TaskTracker so the in-memory work tree
// stays in sync with demarkus.
type ProjectPhaseAddTool struct {
	tracker TaskMutator
}

// NewProjectPhaseAddTool creates a project_phase_add tool backed by a
// TaskMutator. Nil tracker disables the tool; the session-backed
// workspace implements TaskTracker (which composes TaskMutator) in
// normal runs.
func NewProjectPhaseAddTool(tracker TaskMutator) *ProjectPhaseAddTool {
	return &ProjectPhaseAddTool{tracker: tracker}
}

type projectPhaseAddArgs struct {
	Phases []string `json:"phases"`
}

// Definition returns the OpenAI-compatible tool schema.
func (t *ProjectPhaseAddTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "project_phase_add",
			Description: "Append one or more top-level phases (H1) to an existing /project.md. Pass a `phases` array of descriptive titles (e.g. ['Polish','Release']); each is auto-numbered as `# Phase N: Title`. Use this when project_task_add reports no matching phase — project_init only seeds phases on a fresh repo and will not add to an existing plan.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"phases": {
						Type:        "array",
						Description: "Phase titles to append, in order. A bare title is auto-numbered; a title already shaped 'Phase N: Title' is used verbatim.",
						Items:       &llm.FunctionParam{Type: "string"},
					},
				},
				Required: []string{"phases"},
			},
		},
	}
}

// Execute appends each phase via the TaskTracker. Partial success is
// the contract (mirrors project_task_add): an invalid, duplicate, or
// rejected entry does not stop the others. Within-batch duplicates are
// caught by case-folded descriptive title — the LLM occasionally emits
// the same phase twice when sketching a plan, and auto-numbering would
// otherwise turn that slip into two near-identical phases the
// substring-matched project_task_add can no longer target unambiguously.
// The result names the full numbered title of each added phase so the
// LLM can reference it in a follow-up project_task_add call.
func (t *ProjectPhaseAddTool) Execute(_ context.Context, call llm.ToolCall) ToolResult {
	var args projectPhaseAddArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return textResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	if len(args.Phases) == 0 {
		return textResult("Error: phases array is required and must not be empty")
	}
	if t.tracker == nil {
		return textResult("Error: task tracking not available")
	}

	seen := make(map[string]int, len(args.Phases))
	var added, failed []string
	for i, raw := range args.Phases {
		title := strings.TrimSpace(raw)
		if title == "" {
			failed = append(failed, fmt.Sprintf("phases[%d]: title is required", i))
			continue
		}
		key := strings.ToLower(title)
		if firstIdx, dup := seen[key]; dup {
			failed = append(failed, fmt.Sprintf("phases[%d] (%s): duplicate of phases[%d]", i, title, firstIdx))
			continue
		}
		full, err := t.tracker.AddPhase(title)
		if err != nil {
			// Do not register in `seen` on failure — the first occurrence
			// never landed, so a later identical entry is a legitimate retry.
			failed = append(failed, fmt.Sprintf("phases[%d] (%s): %v", i, title, err))
			continue
		}
		seen[key] = i
		added = append(added, full)
	}

	return textResult(formatPhaseAddResult(added, failed))
}

// formatPhaseAddResult composes the tool result with one section per
// outcome. Listing added phases by their full numbered title lets the
// LLM verify the exact heading project_task_add must reference.
func formatPhaseAddResult(added, failed []string) string {
	var b strings.Builder
	if len(added) > 0 {
		fmt.Fprintf(&b, "Added %d phase(s):\n", len(added))
		for _, a := range added {
			fmt.Fprintf(&b, "  - %s\n", a)
		}
	}
	if len(failed) > 0 {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "Failed %d phase(s):\n", len(failed))
		for _, f := range failed {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
	}
	if b.Len() == 0 {
		// Defensive: empty Phases already returns an error above, so this
		// branch is unreachable today. Kept so a future refactor that
		// loosens the empty-array check fails closed instead of silently
		// returning "" to the LLM.
		return "Error: no phases processed"
	}
	return strings.TrimRight(b.String(), "\n")
}
