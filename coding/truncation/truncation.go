// Package truncation handles LLM-provider response truncation.
//
// When a provider cuts off a response mid-stream (hits its max-output-
// token cap), the agent must do three things in order:
//
//  1. Refuse to execute any tool calls in the truncated message —
//     accumulated arguments may be incomplete and silently applying
//     them would corrupt files.
//  2. Append a tool-role reply for every pending tool_calls entry so
//     the transcript stays well-formed (chat/completion validators
//     reject dangling tool_calls on the next request).
//  3. Try to give the next attempt more headroom: when the provider
//     supports runtime escalation, double its max-tokens cap (or seed
//     it at [InitialEscalation] when previously unset), capped at
//     [Ceiling].
//
// After [MaxRetries] consecutive truncations the run abandons the
// turn rather than loop against a model that will not fit in-budget.
//
// This package owns the pure data + math (constants, message
// formatting, escalation arithmetic) and the [Recover] orchestration.
// Side effects flow through narrow ports: an [Escalator] (the LLM
// provider, when it implements the optional capability) and a
// [Sender] func for the status-bar AgentError event. The agent
// retains the runID/budget/sessionUsage state and the inner-turn
// retry counter; this package does not see them.
package truncation

import (
	"fmt"
	"log/slog"

	"github.com/latebit-io/junto/ai/llm"
	"github.com/latebit-io/junto/engine/event"
)

// MaxRetries bounds consecutive truncated turns before [Recover]
// returns a terminal error. Three total attempts is enough for the
// usual escalation path (default → InitialEscalation → Ceiling) plus
// one final "split the work" nudge; beyond that a looping or
// malfunctioning model would just burn requests. Reset to zero on
// the first non-truncated turn so long sessions with occasional
// truncations don't accumulate toward the limit (the agent's run
// loop owns that reset; this package is stateless).
const MaxRetries = 2

// InitialEscalation is the starting value used when [Escalate]
// detects a provider whose max-tokens was previously unset (e.g.
// OpenAI-compat defaults to "unset"). Chosen to give the model
// meaningful extra room while staying below typical per-model caps
// that would reject the request outright.
const InitialEscalation = 32768

// Ceiling bounds escalation to prevent unbounded doubling against
// models that will never honour it. 65536 matches the largest output
// cap supported by current frontier models as of this writing.
const Ceiling = 65536

// Escalator is the optional capability LLM providers implement so
// the agent can bump their output-token cap after a truncated turn.
// [Escalate] no-ops when the active provider does not satisfy it.
type Escalator interface {
	MaxTokens() int
	SetMaxTokens(int)
}

// Compile-time assertions that every shipped llm.Provider continues
// to satisfy [Escalator]. The agent dispatches via type assertion at
// runtime; without these declarations a renamed method on any
// implementation would silently degrade to "escalation unsupported"
// and the agent would loop against the same truncation ceiling. The
// assertion forces the regression to surface at build time.
var (
	_ Escalator = (*llm.AgentAPI)(nil)
	_ Escalator = (*llm.AnthropicAPI)(nil)
	_ Escalator = (*llm.CodexAPI)(nil)
)

// EscalationOutcome describes what happened when [Recover] tried to
// bump the provider's max-tokens cap. The three states feed
// [RecoveryMessages] so the model and the status bar see phrasing
// that matches the actual situation — telling the LLM "the provider
// is at its ceiling" when the provider simply doesn't support
// runtime escalation is misleading and was a real bug in the
// pre-carve agent code.
type EscalationOutcome int

const (
	// EscalationApplied means the cap moved from a lower value to a
	// higher one — the next turn has more headroom.
	EscalationApplied EscalationOutcome = iota
	// EscalationAtCeiling means the provider supports escalation but
	// is already at [Ceiling]. Splitting the work is the only path
	// forward.
	EscalationAtCeiling
	// EscalationUnsupported means the provider does not implement
	// [Escalator] at runtime. Splitting the work is also the only
	// path forward, but the cause is provider capability, not a cap
	// the agent has already exhausted.
	EscalationUnsupported
)

// Sender adapts the agent's event emitter so [Recover] can surface
// status-bar AgentError events without depending on the agent's full
// send/sendCritical surface.
type Sender func(event.Event)

// EscalateValue returns the next max-tokens value for a provider
// whose previous turn was truncated. Doubles the current value,
// starting at [InitialEscalation] when unset, capped at [Ceiling].
// Returns the input unchanged when already at or above the ceiling.
// Pure: no side effects, suitable for direct unit testing.
func EscalateValue(current int) int {
	if current >= Ceiling {
		return Ceiling
	}
	next := current * 2
	if next < InitialEscalation {
		next = InitialEscalation
	}
	if next > Ceiling {
		next = Ceiling
	}
	return next
}

// Escalate doubles the provider's max-tokens cap (best effort). The
// returned bool is false when the provider doesn't move (already at
// the ceiling) or doesn't support escalation at all — the caller can
// use that signal to phrase the recovery message differently.
func Escalate(p Escalator) (from, to int, escalated bool) {
	from = p.MaxTokens()
	to = EscalateValue(from)
	if to == from {
		return from, to, false
	}
	p.SetMaxTokens(to)
	return from, to, true
}

// AppendRejections appends a tool-role reply for each pending tool
// call from a truncated assistant message. Chat-completion transcripts
// require a tool-role message for every tool_calls entry before the
// next assistant turn — skipping them leaves dangling references that
// providers validate and reject on the following request. No-op when
// toolCalls is empty (the assistant message had no tool calls to
// answer).
func AppendRejections(messages []llm.Message, toolCalls []llm.ToolCall, reply string) []llm.Message {
	for _, tc := range toolCalls {
		messages = append(messages, llm.Message{
			Role:       "tool",
			ToolCallID: tc.ID,
			Content:    reply,
		})
	}
	return messages
}

// RecoveryMessages builds the three user-facing strings for a
// truncation event: the tool-role reply (one per pending tool call),
// the user-role nudge (for turns with no tool calls — the loop needs
// SOMETHING to condition the retry on), and the status-bar AgentError
// text. The phrasing differs by [EscalationOutcome] so the model and
// the developer see the actual cause: cap-just-bumped, cap-already-
// at-ceiling, or runtime-escalation-unsupported.
func RecoveryMessages(from, to int, outcome EscalationOutcome) (toolMsg, userMsg, uiMsg string) {
	switch outcome {
	case EscalationApplied:
		toolMsg = fmt.Sprintf(
			"Error: your response was truncated at the model's max output token limit (was %d, now bumped to %d for the next turn). Any arguments accumulated for this tool call are likely incomplete and were NOT executed. Retry — you now have more output headroom, but still prefer narrow edit_file search/replace over full-file rewrites.",
			from, to)
		userMsg = fmt.Sprintf(
			"Your previous response was truncated at the max output token limit. The cap has been raised from %d to %d for this turn — retry with the same plan.",
			from, to)
		uiMsg = fmt.Sprintf("LLM output truncated — bumping max_tokens %d → %d and retrying.", from, to)
	case EscalationAtCeiling:
		toolMsg = "Error: your response was truncated at the model's max output token limit, and the provider is already at its output-cap ceiling. Any arguments accumulated for this tool call are likely incomplete and were NOT executed. You must break the work into smaller pieces — e.g. for edit_file, narrow the search/replace to only the lines that actually change rather than rewriting large blocks."
		userMsg = "Your previous response was truncated at the max output token limit, and the provider is already at its output-cap ceiling. Retry by breaking the work into smaller pieces."
		uiMsg = "LLM output truncated and provider is at max_tokens ceiling — asking the model to split the work."
	case EscalationUnsupported:
		toolMsg = "Error: your response was truncated at the model's max output token limit, and this provider does not support runtime escalation of its output cap. Any arguments accumulated for this tool call are likely incomplete and were NOT executed. You must break the work into smaller pieces — e.g. for edit_file, narrow the search/replace to only the lines that actually change rather than rewriting large blocks."
		userMsg = "Your previous response was truncated at the max output token limit, and this provider does not support runtime cap escalation. Retry by breaking the work into smaller pieces."
		uiMsg = "LLM output truncated and provider does not support runtime cap escalation — asking the model to split the work."
	}
	return
}

// abortReply is the fixed tool-role reply written for every pending
// tool call when [Recover] runs out of retries. Distinct from the
// per-attempt RecoveryMessages strings because the abort is terminal
// — the LLM should know it is being abandoned, not asked to retry.
const abortReply = "Error: your response was truncated at the model's max output token limit, and the agent has exhausted its truncation-recovery retries. The turn is being abandoned to avoid looping against a model that cannot fit its answer in the available budget."

// Recover decides what to do with a truncated turn. Inputs are the
// running message slice, the truncated assistant message's pending
// tool calls, the per-run retry counter, the active LLM provider,
// and a [Sender] for status-bar AgentError events.
//
// When retries < [MaxRetries], appends recovery messages (rejections
// for each tool call plus a user-role nudge if there were no tool
// calls), best-effort escalates the provider's max-tokens cap, and
// returns (newMessages, retries+1, nil) — the run loop continues.
//
// When retries >= [MaxRetries], appends the terminal abort reply for
// each tool call, emits an AgentError, and returns (newMessages,
// retries, terminalErr). The caller must end the turn on this error;
// looping again would re-trigger the abort on every iteration.
//
// provider may be nil (or any value not implementing [Escalator]) —
// escalation simply no-ops in that case.
func Recover(
	messages []llm.Message,
	toolCalls []llm.ToolCall,
	retries int,
	provider llm.Provider,
	sender Sender,
) ([]llm.Message, int, error) {
	if retries >= MaxRetries {
		messages = AppendRejections(messages, toolCalls, abortReply)
		err := fmt.Errorf("agent: abandoning turn after %d consecutive truncated responses", retries+1)
		slog.Error("agent: truncation retries exhausted", "retries", retries)
		if sender != nil {
			sender(event.AgentError{Err: err.Error()})
		}
		return messages, retries, err
	}

	var from, to int
	outcome := EscalationUnsupported
	if esc, ok := provider.(Escalator); ok {
		var moved bool
		from, to, moved = Escalate(esc)
		if moved {
			outcome = EscalationApplied
		} else {
			outcome = EscalationAtCeiling
		}
	}
	slog.Warn("agent: LLM output truncated, rejecting tool calls",
		"tool_calls", len(toolCalls),
		"max_tokens_from", from, "max_tokens_to", to, "outcome", outcome)

	toolMsg, userMsg, uiMsg := RecoveryMessages(from, to, outcome)
	if sender != nil {
		sender(event.AgentError{Err: uiMsg})
	}
	messages = AppendRejections(messages, toolCalls, toolMsg)
	if len(toolCalls) == 0 {
		messages = append(messages, llm.Message{
			Role:    "user",
			Content: userMsg,
		})
	}
	return messages, retries + 1, nil
}
