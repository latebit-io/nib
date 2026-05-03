package agent

import (
	"fmt"

	"github.com/latebit-io/nib/engine/project"
)

// projectMDPath is the canonical demarkus path of the coding agent's
// strict project.md document. The path is coding-specific — the schema
// enforced for it is the task-tree contract in [engine/project], which
// no other kit consumer cares about.
const projectMDPath = "/project.md"

// llmError wraps an LLM-facing message as an error. The message ends
// up in the tool result body verbatim, so it is intentionally written
// in user-readable prose with sentence-ending punctuation; that
// violates Go's ST1005 convention for developer-facing errors but is
// correct for this surface. The named type isolates the suppression
// to one declaration instead of sprinkling //nolint at every return.
type llmError string

func (e llmError) Error() string { return string(e) }

// publishProjectMDValidator returns a validator that runs the strict
// project.md schema check on writes targeting [projectMDPath] and
// passes everything else through. Wired into the kit memory_publish
// tool at construction time so the LLM gets a deterministic schema
// rejection without round-tripping through the store.
func publishProjectMDValidator(path, body string) error {
	if path != projectMDPath {
		return nil
	}
	if errs := project.Validate(project.Parse(body)); errs != nil {
		return llmError(fmt.Sprintf(
			"Error: %s rejected — schema violations below. Fix all and retry.\n%s",
			projectMDPath,
			errs.Error(),
		))
	}
	return nil
}

// appendProjectMDValidator returns an error for any append targeting
// [projectMDPath] — raw appends to the strict-schema document would
// break the task tree, so the agent is steered toward the structured
// task tools instead. Other paths pass through.
func appendProjectMDValidator(path, _ string) error {
	if path != projectMDPath {
		return nil
	}
	return llmError(
		"Error: raw append to " + projectMDPath + " is not allowed — it would break the strict schema. " +
			"Use project_task_add (for new tasks) or update_task (to activate/complete) instead.",
	)
}
