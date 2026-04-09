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

// StatusKind is a typed enum for agent status values.
// It uses string constants so debug output remains human-readable.
type StatusKind string

const (
	// StatusIdle means the agent is not active.
	StatusIdle StatusKind = "idle"
	// StatusThinking means the agent is processing / waiting on the LLM.
	StatusThinking StatusKind = "thinking"
	// StatusPlanning means the agent is in planning mode.
	StatusPlanning StatusKind = "planning"
	// StatusPlanningWaiting means planning is done and the agent awaits user action.
	StatusPlanningWaiting StatusKind = "planning-waiting"
	// StatusReviewing means an edit proposal is pending user review.
	StatusReviewing StatusKind = "reviewing"
	// StatusEditing means an approved edit is being applied and the user may continue.
	StatusEditing StatusKind = "editing"
	// StatusWaiting means the agent is waiting for user input.
	StatusWaiting StatusKind = "waiting"
	// StatusTyping means the agent is animating typed text into the editor.
	StatusTyping StatusKind = "typing"
	// StatusLinting means the agent is running post-edit style lint commands.
	StatusLinting StatusKind = "linting"
)

// AgentStatus updates the agent status display.
type AgentStatus struct{ Status StatusKind }

// AgentToolCall signals the agent is invoking a tool.
// Emitted before execution so the frontend can show what the agent is doing.
type AgentToolCall struct {
	Name string // tool name (e.g. "search_project", "read_file")
	Args string // raw JSON arguments
}

// AgentNavigate signals the agent wants to navigate the editor to a location.
// The frontend handles the actual cursor movement on its own goroutine.
type AgentNavigate struct {
	Path string // file to navigate to
	Line int    // 1-indexed line number
}

// AgentStyleRejected signals that the style evaluator rejected a proposed
// edit. The frontend should dismiss the diff overlay. Violation explanations
// are sent separately via AgentToken events.
type AgentStyleRejected struct {
	Path string // file that was being edited
}

func (AgentToken) eventTag()         {}
func (AgentEditProposed) eventTag()  {}
func (AgentStyleRejected) eventTag() {}
func (AgentFileCreated) eventTag()   {}
func (AgentToolCall) eventTag()      {}
func (AgentNavigate) eventTag()      {}

// AgentWaiting signals the agent finished its turn and is waiting for user input.
// The frontend should enable the input prompt so the developer can continue
// the conversation. The agent goroutine is blocked until Reply() is called.
type AgentWaiting struct{}

func (AgentDone) eventTag()    {}
func (AgentError) eventTag()   {}
func (AgentStatus) eventTag()  {}
func (AgentWaiting) eventTag() {}

// PendingEdit is a proposed edit from the LLM, sent to the frontend for approval.
type PendingEdit struct {
	ID      string
	Path    string // which file this edit targets
	Search  string
	Replace string
	Reason  string
}

// FlushResult carries the outcome of a FlushBuffers request.
type FlushResult struct {
	Saved []string // canonical paths of files that were saved
	Err   error    // first error encountered (nil on full success)
}

// FlushBuffers requests the frontend to save all dirty buffers to disk.
// The agent blocks on Result until the frontend completes the save.
// This routes the I/O through the buffer-owning goroutine (TUI main)
// so that no cross-goroutine buffer access occurs.
type FlushBuffers struct {
	Result chan<- FlushResult
}

func (FlushBuffers) eventTag() {}

// --- Language service events ---

// DiagnosticsUpdated signals that diagnostics changed for a file.
// Frontend should re-query the DiagnosticProvider for the updated diagnostics.
type DiagnosticsUpdated struct{ Path string }

func (DiagnosticsUpdated) eventTag() {}
