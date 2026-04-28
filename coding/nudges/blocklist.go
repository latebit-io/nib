package nudges

import "maps"

// planningBlocklistDefaults contains the tool names disabled during
// planning mode. These are write-side tools that modify code or run
// commands.
//
// smoke_run is here because it executes the project's smoke command
// (typically `make smoke` / `lua main.lua` / etc.) via `sh -c`. That
// is command execution — same threat profile as bash — and planning
// mode is supposed to be read-only. The auto-invocation path in
// runTaskReview only fires from update_task(complete), which is itself
// blocked here, so the auto-path is naturally suppressed in planning
// mode too.
//
// Unexported because exporting a map exports a mutation surface — a
// single `nudges.X["bash"] = false` from any importer would weaken
// planning enforcement globally for every Agent constructed after.
// Callers go through [DefaultPlanningBlocklist] which returns a fresh
// clone they can mutate freely.
var planningBlocklistDefaults = map[string]bool{
	"edit_file":    true,
	"write_file":   true,
	"replace_file": true,
	"bash":         true,
	"smoke_run":    true,
	"update_task":  true,
}

// DefaultPlanningBlocklist returns a fresh copy of the built-in
// planning blocklist. Callers may mutate the returned map (e.g. to
// merge in extra tool names from NewOptions.PlanningBlocklist) without
// affecting the package-level defaults or other Agent instances.
func DefaultPlanningBlocklist() map[string]bool {
	return maps.Clone(planningBlocklistDefaults)
}
