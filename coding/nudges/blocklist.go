package nudges

// PlanningBlocklist contains tool names disabled during planning mode.
// These are write-side tools that modify code or run commands.
//
// smoke_run is here because it executes the project's smoke command
// (typically `make smoke` / `lua main.lua` / etc.) via `sh -c`. That
// is command execution — same threat profile as bash — and planning
// mode is supposed to be read-only. The auto-invocation path in
// runTaskReview only fires from update_task(complete), which is itself
// blocked here, so the auto-path is naturally suppressed in planning
// mode too.
//
// The map is read-only at runtime; the agent copies it into a
// per-instance map so callers can extend the blocklist via
// NewOptions.PlanningBlocklist without mutating the package-level
// default.
var PlanningBlocklist = map[string]bool{
	"edit_file":    true,
	"write_file":   true,
	"replace_file": true,
	"bash":         true,
	"smoke_run":    true,
	"update_task":  true,
}
