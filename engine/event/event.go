// Package event defines the unified event types for engine→frontend communication.
// All producers (agent, language service, etc.) write to a single chan Event.
// Frontends read one channel and type-switch to handle each event.
package event

// Event is the sealed interface for all engine→frontend communication.
// Only types in this package implement it.
type Event interface {
	eventTag()
}

// --- Agent mode ---

// Mode controls the agent's behavior — which tools are available and
// which prompts are used. Defined in event (the shared domain types package)
// so both session and agent can import it without circular dependencies.
type Mode int

const (
	// ModeExecution is the default mode: all tools available, execution prompt.
	ModeExecution Mode = iota
	// ModePlanning restricts the agent to read-only and memory tools,
	// using a planning-focused prompt for conversational design.
	ModePlanning
)

// --- Agent events ---

// AgentToken delivers streaming text from the LLM.
type AgentToken struct {
	// Text is the text delta from the LLM stream.
	Text string
}

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

// AgentDone signals the agent loop has finished.
// Success is true when the loop completed normally (not cancelled or errored).
type AgentDone struct {
	// Success is true when the loop completed normally.
	Success bool
}

// AgentError carries an error from the agent.
type AgentError struct {
	// Err is the error message.
	Err string
}

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
	// StatusBlockReview means an edit proposal is pending user review
	// SPECIFICALLY because a validator stage flagged it (architecture
	// cap, lint, etc.). The TUI uses this to render a more
	// attention-grabbing status indicator than plain "reviewing" —
	// the developer needs to know this surfaced for a reason and is
	// not the routine review-and-approve flow they'd see at lower
	// autonomy levels.
	StatusBlockReview StatusKind = "block-review"
	// StatusWaiting means the agent is waiting for user input.
	StatusWaiting StatusKind = "waiting"
	// StatusFinished is a turn-end idle state like StatusWaiting, but the
	// agent yielded after declaring its tracked task tree empty (sanctioned
	// stop condition #1). Distinct surface so the developer can tell at a
	// glance whether the pause is "your turn to reply" or "I think all work
	// is done — type a new goal or close the session." Both states accept
	// the same input flow; the difference is purely diagnostic.
	StatusFinished StatusKind = "finished"
	// StatusLinting means the agent is running post-edit style lint commands.
	StatusLinting StatusKind = "linting"
)

// AgentStatus updates the agent status display.
type AgentStatus struct {
	// Status is the current agent status kind.
	Status StatusKind
}

// AgentToolCall signals the agent is invoking a tool.
// Emitted before execution so the frontend can show what the agent is doing.
type AgentToolCall struct {
	// Name is the tool name (e.g., "search_project", "read_file").
	Name string
	// Args is the raw JSON arguments for the tool call.
	Args string
}

// AgentNavigate signals the agent wants to navigate the editor to a location.
// The frontend handles the actual cursor movement on its own goroutine.
type AgentNavigate struct {
	// Path is the file to navigate to.
	Path string
	// Line is the 1-indexed line number to navigate to.
	Line int
}

func (AgentToken) eventTag()        {}
func (AgentEditProposed) eventTag() {}
func (AgentFileCreated) eventTag()  {}
func (AgentToolCall) eventTag()     {}
func (AgentNavigate) eventTag()     {}

// AgentWaiting signals the agent finished its turn and is waiting for user input.
// The frontend should enable the input prompt so the developer can continue
// the conversation. The agent goroutine is blocked until Reply() is called.
type AgentWaiting struct {
	// Finished is true when the agent yielded after declaring its tracked
	// task tree empty (sanctioned stop condition #1 — all tasks complete).
	// Frontends can use this to distinguish "your turn to reply" from
	// "I'm done with the planned work" without changing the input flow.
	Finished bool
}

func (AgentDone) eventTag()    {}
func (AgentError) eventTag()   {}
func (AgentStatus) eventTag()  {}
func (AgentWaiting) eventTag() {}

// AgentTurnUsage reports token consumption for a single agent turn
// (one or more LLM calls within processLLMTurn). Combines provider-reported
// exact counts with client-side composition estimates.
type AgentTurnUsage struct {
	// Turn is the 1-indexed turn number within this agent run.
	Turn int
	// PromptTokens is the provider-reported total input tokens (0 if unavailable).
	PromptTokens int
	// CompletionTokens is the provider-reported output tokens (0 if unavailable).
	CompletionTokens int
	// CachedTokens is the provider-reported cached input tokens (0 if unavailable).
	CachedTokens int
	// ToolCalls is the number of tool calls dispatched in this turn.
	ToolCalls int

	// Client-side estimates (always available).
	// SystemEst is the estimated system prompt tokens.
	SystemEst int
	// ToolsEst is the estimated tool definition tokens.
	ToolsEst int
	// HistoryEst is the estimated conversation history tokens.
	HistoryEst int
	// NewEst is the estimated new input tokens.
	NewEst int
	// CompletionEst is the estimated output tokens (from streamed content length).
	CompletionEst int
}

func (AgentTurnUsage) eventTag() {}

// AgentInputEstimate signals the estimated input token composition before
// an LLM call starts. Sent right before Stream() so the frontend can show
// real-time input cost while the response is streaming.
type AgentInputEstimate struct {
	// System is the estimated system prompt tokens.
	System int
	// Tools is the estimated tool definition tokens.
	Tools int
	// History is the estimated conversation history tokens.
	History int
	// New is the estimated new input tokens.
	New int
}

func (AgentInputEstimate) eventTag() {}

// AgentCompacted signals that conversation history was compacted to reduce
// token usage. Emitted once per compaction pass, before the next LLM call.
type AgentCompacted struct {
	// BeforeTokens is the estimated history tokens before compaction.
	BeforeTokens int
	// AfterTokens is the estimated history tokens after compaction.
	AfterTokens int
}

func (AgentCompacted) eventTag() {}

// ReloadBuffers requests the frontend to re-read all open buffers from disk.
// Sent after bash tool calls that may have modified files outside the edit
// approval flow. The frontend should reload buffers whose on-disk content
// differs from the in-memory content.
type ReloadBuffers struct{}

func (ReloadBuffers) eventTag() {}

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

// FlushResult carries the outcome of a FlushBuffers request.
type FlushResult struct {
	// Saved lists the canonical paths of files that were saved.
	Saved []string
	// Err is the first error encountered (nil on full success).
	Err error
}

// FlushBuffers requests the frontend to save all dirty buffers to disk.
// The agent blocks on Result until the frontend completes the save.
// This routes the I/O through the buffer-owning goroutine (the frontend's
// main loop) so that no cross-goroutine buffer access occurs.
type FlushBuffers struct {
	// Result receives the flush outcome from the frontend.
	Result chan<- FlushResult
}

func (FlushBuffers) eventTag() {}

// --- Language service events ---

// DiagnosticsUpdated signals that diagnostics changed for a file.
// Frontend should re-query the DiagnosticProvider for the updated diagnostics.
type DiagnosticsUpdated struct {
	// Path is the file whose diagnostics changed.
	Path string
}

func (DiagnosticsUpdated) eventTag() {}
