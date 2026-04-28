package agent

import "testing"

// TestPlanningBlocklistContents locks the planning-mode blocklist
// against silent expansion or contraction. Adding a new write-side
// tool (anything that mutates files, runs shell, or commits state)
// must extend this list — and removing any entry must be deliberate.
//
// `smoke_run` is on the list because it executes the project's smoke
// command via `sh -c` (same threat profile as `bash`); planning mode
// is supposed to be read-only with no command execution.
func TestPlanningBlocklistContents(t *testing.T) {
	t.Parallel()

	want := map[string]bool{
		"edit_file":    true,
		"write_file":   true,
		"replace_file": true,
		"bash":         true,
		"smoke_run":    true,
		"update_task":  true,
	}

	if len(planningBlocklist) != len(want) {
		t.Errorf("planningBlocklist size = %d, want %d (members: %v vs %v)",
			len(planningBlocklist), len(want), planningBlocklist, want)
	}
	for name, expected := range want {
		if got := planningBlocklist[name]; got != expected {
			t.Errorf("planningBlocklist[%q] = %v, want %v", name, got, expected)
		}
	}
	for name := range planningBlocklist {
		if !want[name] {
			t.Errorf("planningBlocklist contains unexpected entry %q — add to test or revert", name)
		}
	}
}
