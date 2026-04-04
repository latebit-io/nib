package session

// Phase represents the current workflow phase of the session.
type Phase int

const (
	// PhaseNone is the default — no active workflow.
	PhaseNone Phase = iota
	// PhasePlanning indicates a planning conversation is in progress.
	// Write-side tools are disabled; the agent discusses design with the developer.
	PhasePlanning
	// PhaseExecution indicates the agent is coding — all tools available.
	PhaseExecution
)
