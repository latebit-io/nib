// Package event defines the application-domain events emitted by the
// coding agent and consumed by frontends (TUI, headless runner, tests).
//
// Coding-specific events live here. Generic agent events (streaming
// tokens, lifecycle, status, usage, mode) live in
// [github.com/latebit-io/nib/kit/event] and are re-exported as type
// aliases below so existing callers continue to type [event.AgentToken],
// [event.AgentDone], etc. without churn.
package event

import (
	kitevent "github.com/latebit-io/nib/kit/event"
)

// Event is the marker interface for the coding agent's frontend
// channel. Aliased to [kitevent.Event] so generic kit events and the
// coding-specific events declared below flow on the same channel.
//
// Intentionally NOT sealed at this layer. The kit's marker method
// ([kitevent.Event.Event]) is exported by design so library consumers
// and third-party tools can declare their own event types; aliasing
// (rather than embedding-plus-private-marker) preserves that openness.
// Sealing here would break the unified channel — a [kitevent.AgentToken]
// must satisfy [Event] for streaming text from the LLM to reach the
// frontend without conversion. Frontends that consume this channel do
// so via type-switch and should default-case unknown event types.
type Event = kitevent.Event

// --- Aliased types from kit/event ---

// Mode is the agent execution/planning mode. Aliased to [kitevent.Mode].
type Mode = kitevent.Mode

const (
	// ModeExecution is the default mode: all tools available, execution prompt.
	ModeExecution = kitevent.ModeExecution
	// ModePlanning restricts the agent to read-only and memory tools.
	ModePlanning = kitevent.ModePlanning
)

// AgentToken is the streaming-text-delta event. Aliased to [kitevent.AgentToken].
type AgentToken = kitevent.AgentToken

// AgentDone is the agent-loop-finished event. Aliased to [kitevent.AgentDone].
type AgentDone = kitevent.AgentDone

// AgentError is the agent-error event. Aliased to [kitevent.AgentError].
type AgentError = kitevent.AgentError

// AgentToolCall is the tool-invocation event. Aliased to [kitevent.AgentToolCall].
type AgentToolCall = kitevent.AgentToolCall

// AgentWaiting is the turn-yield event. Aliased to [kitevent.AgentWaiting].
type AgentWaiting = kitevent.AgentWaiting

// AgentStatus is the status update event. Aliased to [kitevent.AgentStatus].
type AgentStatus = kitevent.AgentStatus

// AgentTurnUsage is the per-turn token-usage event. Aliased to [kitevent.AgentTurnUsage].
type AgentTurnUsage = kitevent.AgentTurnUsage

// AgentInputEstimate is the pre-stream input estimate event.
// Aliased to [kitevent.AgentInputEstimate].
type AgentInputEstimate = kitevent.AgentInputEstimate

// AgentCompacted is the history-compacted event. Aliased to [kitevent.AgentCompacted].
type AgentCompacted = kitevent.AgentCompacted

// StatusKind is the typed enum for agent status values. Aliased to
// [kitevent.StatusKind] so coding-specific constants below extend the
// same nominal type as the generic ones in [kitevent].
type StatusKind = kitevent.StatusKind

const (
	// StatusIdle means the agent is not active.
	StatusIdle = kitevent.StatusIdle
	// StatusThinking means the agent is processing / waiting on the LLM.
	StatusThinking = kitevent.StatusThinking
	// StatusPlanning means the agent is in planning mode.
	StatusPlanning = kitevent.StatusPlanning
	// StatusPlanningWaiting means planning is done and the agent awaits user action.
	StatusPlanningWaiting = kitevent.StatusPlanningWaiting
	// StatusWaiting means the agent is waiting for user input.
	StatusWaiting = kitevent.StatusWaiting
	// StatusFinished marks a turn-end where the agent declared its task tree empty.
	StatusFinished = kitevent.StatusFinished
)

// --- Coding-specific status constants ---

const (
	// StatusReviewing means an edit proposal is pending user review.
	StatusReviewing StatusKind = "reviewing"
	// StatusLinting means the agent is running post-edit style lint commands.
	StatusLinting StatusKind = "linting"
	// StatusSmoke means the agent is running the post-task smoke command to
	// verify the artifact actually launches. Distinct from StatusLinting so
	// the frontend does not show a stale "linting" phase during the smoke
	// run, which can block for the lifetime of the launched process.
	StatusSmoke StatusKind = "smoke"
)

// --- Coding-specific events ---

// ValidatorSummary is the frontend-facing projection of a pre-approval
// validator result. The full validate.Result type is intentionally NOT
// exported here — this struct carries only the fields a UI or capture
// adapter needs, so the event wire does not couple to the validate
// package's internals.
type ValidatorSummary struct {
	// Stage identifies the validator (e.g. "go-parse", "tree-sitter").
	Stage string
	// Verdict is the string form ("pass", "retry", "block").
	Verdict string
	// Feedback is the human-readable reason the validator returned this
	// verdict. Empty on Pass. Populated on Retry (the LLM gets it back
	// as a tool-result retry prompt) and on Block (the developer sees
	// it in the review banner so they know what rule fired). The same
	// string serves both audiences — kept terse and self-contained so
	// it reads as well in a chat panel as in a turn replay.
	Feedback string
}

// AgentEditProposed signals the agent wants to apply an edit.
type AgentEditProposed struct {
	// Edit is the proposed edit awaiting approval.
	Edit PendingEdit
	// ValidatorSummaries reports the pre-approval validator outcomes,
	// in the order the pipeline executed them. Empty when no validators
	// ran or when no pipeline is installed. Consumers unaware of the
	// field continue to work as before.
	ValidatorSummaries []ValidatorSummary
}

// AgentFileCreated signals the agent created a new file.
type AgentFileCreated struct {
	// Path is the absolute path of the newly created file.
	Path string
}

// AgentNavigate signals the agent wants to navigate the editor to a
// location. The frontend handles the actual cursor movement on its own
// goroutine — Session never holds a UI cursor.
type AgentNavigate struct {
	// Path is the file to navigate to.
	Path string
	// Line is the 1-indexed line number to navigate to.
	Line int
	// Col is the 0-indexed rune column to land on. Zero is a valid
	// "start of line" target; consumers that previously emitted only
	// (path, line) interpret Col=0 as the original behavior.
	Col int
}

// ReloadBuffers requests the frontend to re-read all open buffers from disk.
// Sent after bash tool calls that may have modified files outside the edit
// approval flow.
//
// Contract: the frontend MUST skip any buffer with unsaved in-editor changes.
// "On-disk differs from in-memory" is true for every dirty buffer; a literal
// reload-on-diff would clobber the developer's work. Reload only clean
// buffers; dirty buffers are the developer's source of truth and a frontend
// that wants to surface the conflict should do so explicitly rather than
// silently overwrite. The reference TUI implementation (handleFileChanged)
// gates on the buffer's modified flag for exactly this reason.
type ReloadBuffers struct{}

// AgentCommandProposed signals the agent wants to run a shell command
// and is blocked awaiting per-command approval. Delivery is critical
// (like [AgentEditProposed]): a frontend that receives it must answer
// with Approve or Reject or the run stalls until cancellation.
type AgentCommandProposed struct {
	// Command is the proposed command awaiting approval.
	Command PendingCommand
}

// PendingCommand is a proposed shell command from the LLM, sent to the
// frontend for approval before the bash tool executes it.
type PendingCommand struct {
	// ID uniquely identifies this command proposal (the LLM tool-call ID).
	ID string
	// Command is the exact shell command string the agent wants to run.
	Command string
	// Reason is the guard classification that made the command
	// approval-worthy (e.g. "destructive command"). Empty for a plain
	// command that is simply not on the always-allow list.
	Reason string
}

// PendingEdit is a proposed edit from the LLM, sent to the frontend for approval.
type PendingEdit struct {
	// ID uniquely identifies this edit proposal.
	ID string
	// Path is the file this edit targets.
	Path string
	// Search is the exact text to find in the file.
	Search string
	// Replace is the replacement text.
	Replace string
	// Reason is the LLM's explanation for the edit.
	Reason string
}

// --- Editor-domain events mirrored from engine/event ---

// DiagnosticsUpdated signals that diagnostics changed for a file.
// Frontend should re-query the DiagnosticProvider for the updated diagnostics.
//
// Mirrored from engine/event.DiagnosticsUpdated so the application's unified
// event channel carries both engine and coding events. The wire layer fans
// engine/event.DiagnosticsUpdated into this type.
type DiagnosticsUpdated struct {
	// Path is the file whose diagnostics changed.
	Path string
}

// SubagentPhase marks where in a spawned subagent's lifecycle a
// [SubagentActivity] falls.
type SubagentPhase uint8

const (
	// SubagentStarted fires once when a child subagent begins running.
	SubagentStarted SubagentPhase = iota
	// SubagentTool fires for each tool the child invokes (Detail = tool name).
	SubagentTool
	// SubagentFinished fires once when the child completes (Success set).
	SubagentFinished
)

// SubagentActivity reports a spawned subagent's progress, so a frontend can
// render the child's nested activity distinctly from the parent's own
// transcript (a subagent sub-pane) rather than interleaving it inline.
// Emitted by the spawner; a frontend without special handling ignores it
// via the default type-switch case.
type SubagentActivity struct {
	// Name is the subagent definition's name.
	Name string
	// Phase is the lifecycle point this activity marks.
	Phase SubagentPhase
	// Detail is phase-specific: the tool name for [SubagentTool]; an
	// optional summary for [SubagentFinished]; empty otherwise.
	Detail string
	// Success is meaningful only for [SubagentFinished].
	Success bool
}

// --- Marker method implementations ---

// Event marks AgentEditProposed as an agent event.
func (AgentEditProposed) Event() {}

// Event marks AgentCommandProposed as an agent event.
func (AgentCommandProposed) Event() {}

// Event marks AgentFileCreated as an agent event.
func (AgentFileCreated) Event() {}

// Event marks AgentNavigate as an agent event.
func (AgentNavigate) Event() {}

// Event marks ReloadBuffers as an agent event.
func (ReloadBuffers) Event() {}

// Event marks DiagnosticsUpdated as an agent event.
func (DiagnosticsUpdated) Event() {}

// Event marks SubagentActivity as an agent event.
func (SubagentActivity) Event() {}
