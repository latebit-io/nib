// Package event defines the generic agent → frontend event vocabulary
// shared across every kit-based agent. Coding, research, ops, or any
// other specialization layered on the kit imports this package for the
// [Event] interface and the lifecycle event types it owns; specializations
// add their own domain events that also implement [Event] and flow on
// the same channel.
//
// The interface uses an exported marker method ([Event.Event]) so types
// declared outside this package can join the channel — the price of
// extensibility is that the marker is positively opt-in rather than
// package-sealed.
package event

// Event is the marker interface every type sent on a kit agent's event
// channel implements. The marker method does nothing; types add it to
// declare "I am an event." Frontends consume the channel and type-switch
// on the concrete type to render or react.
type Event interface {
	// Event marks the type as a kit agent event. No-op.
	Event()
}

// --- Agent mode ---

// Mode controls the agent's behavior — which tools are available and
// which prompts are used. Specializations interpret the mode (e.g. a
// coding agent restricts edit tools in planning mode) but the modes
// themselves are generic.
type Mode int

const (
	// ModeExecution is the default mode: all tools available, execution prompt.
	ModeExecution Mode = iota
	// ModePlanning restricts the agent to read-only and memory tools,
	// using a planning-focused prompt for conversational design.
	ModePlanning
)

// --- Streaming + lifecycle events ---

// AgentToken delivers streaming text from the LLM.
type AgentToken struct {
	// Text is the text delta from the LLM stream.
	Text string
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

// AgentToolCall signals the agent is invoking a tool.
// Emitted before execution so the frontend can show what the agent is doing.
type AgentToolCall struct {
	// Name is the tool name (e.g., "search_project", "read_file").
	Name string
	// Args is the raw JSON arguments for the tool call.
	Args string
}

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

// --- Status ---

// StatusKind is a typed enum for agent status values. Specializations may
// declare additional values of this type for status states unique to
// their domain (a coding agent adds StatusReviewing, StatusLinting,
// etc.). Frontends rendering an unknown StatusKind should fall back to
// the raw string.
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
	// StatusWaiting means the agent is waiting for user input.
	StatusWaiting StatusKind = "waiting"
	// StatusFinished is a turn-end idle state like StatusWaiting, but the
	// agent yielded after declaring its tracked task tree empty (sanctioned
	// stop condition #1). Distinct surface so the developer can tell at a
	// glance whether the pause is "your turn to reply" or "I think all work
	// is done — type a new goal or close the session." Both states accept
	// the same input flow; the difference is purely diagnostic.
	StatusFinished StatusKind = "finished"
)

// AgentStatus updates the agent status display.
type AgentStatus struct {
	// Status is the current agent status kind.
	Status StatusKind
}

// --- Token usage / compaction ---

// AgentTurnUsage reports token consumption for a single agent turn
// (one or more LLM Stream calls). Combines provider-reported exact
// counts with client-side composition estimates.
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

// AgentCompacted signals that conversation history was compacted to reduce
// token usage. Emitted once per compaction pass, before the next LLM call.
type AgentCompacted struct {
	// BeforeTokens is the estimated history tokens before compaction.
	BeforeTokens int
	// AfterTokens is the estimated history tokens after compaction.
	AfterTokens int
}

// --- Marker method implementations ---

func (AgentToken) Event()         {}
func (AgentDone) Event()          {}
func (AgentError) Event()         {}
func (AgentToolCall) Event()      {}
func (AgentWaiting) Event()       {}
func (AgentStatus) Event()        {}
func (AgentTurnUsage) Event()     {}
func (AgentInputEstimate) Event() {}
func (AgentCompacted) Event()     {}
