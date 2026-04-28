package nudges

import "testing"

// TestDefaultPlanningBlocklistContents locks the planning-mode
// blocklist against silent expansion or contraction. Adding a new
// write-side tool (anything that mutates files, runs shell, or commits
// state) must extend this list — and removing any entry must be
// deliberate.
//
// `smoke_run` is on the list because it executes the project's smoke
// command via `sh -c` (same threat profile as `bash`); planning mode
// is supposed to be read-only with no command execution.
func TestDefaultPlanningBlocklistContents(t *testing.T) {
	t.Parallel()

	want := map[string]bool{
		"edit_file":    true,
		"write_file":   true,
		"replace_file": true,
		"bash":         true,
		"smoke_run":    true,
		"update_task":  true,
	}

	got := DefaultPlanningBlocklist()
	if len(got) != len(want) {
		t.Errorf("DefaultPlanningBlocklist size = %d, want %d (members: %v vs %v)",
			len(got), len(want), got, want)
	}
	for name, expected := range want {
		if got[name] != expected {
			t.Errorf("DefaultPlanningBlocklist[%q] = %v, want %v", name, got[name], expected)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("DefaultPlanningBlocklist contains unexpected entry %q — add to test or revert", name)
		}
	}
}

// TestDefaultPlanningBlocklistIsCopy verifies that mutating the map
// returned by DefaultPlanningBlocklist does not bleed into subsequent
// callers. This is the property that motivates the function — without
// it the package-level defaults would be a global mutation surface.
func TestDefaultPlanningBlocklistIsCopy(t *testing.T) {
	t.Parallel()

	first := DefaultPlanningBlocklist()
	first["bash"] = false      // simulate a misbehaving caller flipping a default
	delete(first, "edit_file") // and removing one
	first["new_tool"] = true   // and adding one

	second := DefaultPlanningBlocklist()
	if !second["bash"] {
		t.Error("DefaultPlanningBlocklist returned a shared map: bash flipped after first caller mutated it")
	}
	if !second["edit_file"] {
		t.Error("DefaultPlanningBlocklist returned a shared map: edit_file missing after first caller deleted it")
	}
	if second["new_tool"] {
		t.Error("DefaultPlanningBlocklist returned a shared map: new_tool leaked from first caller")
	}
}
