package headless

import (
	"encoding/json"
	"io"
	"slices"
)

// Result captures the outcome of a headless agent run.
type Result struct {
	// Success is true when the agent completed without errors.
	Success bool `json:"success"`
	// Summary is the agent's accumulated text output (non-tool-call content).
	Summary string `json:"summary"`
	// FilesChanged lists absolute paths of files modified by the agent.
	FilesChanged []string `json:"files_changed"`
	// FilesCreated lists absolute paths of files created by the agent.
	FilesCreated []string `json:"files_created"`
	// Errors collects error messages encountered during the run.
	Errors []string `json:"errors,omitempty"`
}

// WriteJSON encodes the result as indented JSON to w.
func (r *Result) WriteJSON(w io.Writer) error {
	// Sort for deterministic output.
	slices.Sort(r.FilesChanged)
	slices.Sort(r.FilesCreated)

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
