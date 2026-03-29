// Package event defines the unified event types for engine→frontend communication.
// All producers (agent, language service, etc.) write to a single chan Event.
// Frontends read one channel and type-switch to handle each event.
package event

// Event is the sealed interface for all engine→frontend communication.
// Only types in this package implement it.
type Event interface {
	eventTag()
}

// --- Agent events ---

// AgentToken delivers streaming text from the LLM.
type AgentToken struct{ Text string }

// AgentEditProposed signals the agent wants to apply an edit.
type AgentEditProposed struct{ Edit PendingEdit }

// AgentFileCreated signals the agent created a new file.
type AgentFileCreated struct{ Path string }

// AgentDone signals the agent loop has finished.
// Success is true when the loop completed normally (not cancelled or errored).
type AgentDone struct{ Success bool }

// AgentError carries an error from the agent.
type AgentError struct{ Err string }

// AgentStatus updates the agent status display.
type AgentStatus struct{ Status string }

// AgentNavigate signals the agent wants to navigate the editor to a location.
// The frontend handles the actual cursor movement on its own goroutine.
type AgentNavigate struct {
	Path string // file to navigate to
	Line int    // 1-indexed line number
}

func (AgentToken) eventTag()        {}
func (AgentEditProposed) eventTag() {}
func (AgentFileCreated) eventTag()  {}
func (AgentNavigate) eventTag()     {}
func (AgentDone) eventTag()         {}
func (AgentError) eventTag()        {}
func (AgentStatus) eventTag()       {}

// PendingEdit is a proposed edit from the LLM, sent to the frontend for approval.
type PendingEdit struct {
	ID      string
	Path    string // which file this edit targets
	Search  string
	Replace string
	Reason  string
}

// --- Language service events ---

// DiagnosticsUpdated signals that diagnostics changed for a file.
// Frontend should re-query the DiagnosticProvider for the updated diagnostics.
type DiagnosticsUpdated struct{ Path string }

func (DiagnosticsUpdated) eventTag() {}
