// Package streaming handles the LLM I/O lifecycle: consuming the
// provider's stream channel into a single turn's [Result], and
// pruning conversation history before it grows past the cost-control
// threshold. Both halves transform values and emit events through a
// [Sender] callback — they own no run state, no goroutine, and no
// channels of their own.
//
// drain.go owns the per-stream consumer: token forwarding,
// <think>...</think> stripping, byte-cap enforcement, terminal-event
// detection. compact.go owns the per-turn history pruner: token
// estimation against [CompactHistoryThreshold] and dispatch into
// [llm.CompactMessages].
//
// Splitting these out of the agent's run loop keeps the cost-control
// pipeline reviewable without scrolling through 1900 lines of
// orchestration. The agent retains the run state, runID staleness
// guards, and the message slice; this package operates on values.
package streaming

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/latebit-io/junto/ai/llm"
	"github.com/latebit-io/junto/engine/event"
)

// Sender is the agent's event emitter shape. [Drain] forwards stream
// tokens through it and the compaction helpers surface their own
// status events the same way. Defined as a func type so callers can
// pass a method value (e.g. `agent.send`) without an adapter.
type Sender func(event.Event)

// Result aggregates the outcome of one LLM streaming response after
// the terminal Done event is observed. Tokens are forwarded to the
// [Sender] as they arrive; only the final accumulated state flows
// back to the caller via this struct.
type Result struct {
	// Content is the assistant's text content with any <think> spans
	// stripped.
	Content string
	// ToolCalls is the set of tool invocations the assistant emitted
	// in this turn, populated from the terminal Done event.
	ToolCalls []llm.ToolCall
	// Usage carries provider-reported token accounting when present.
	// May be nil for providers that don't report it.
	Usage *llm.Usage
	// Truncated is true when the provider cut the response off at
	// its max-output-token cap. Triggers the truncation-recovery
	// path in the run loop; see [coding/truncation].
	Truncated bool
}

// MaxContentBytes caps the text content accumulated from a single
// streaming response. Matches the SSE scanner and tool-arg caps used
// elsewhere in the LLM layer. A misbehaving provider (e.g. one stuck
// in a regeneration loop) cannot exhaust memory by streaming
// unbounded tokens; once the cap is reached, subsequent tokens are
// dropped and the overflow is logged.
const MaxContentBytes = 10 * 1024 * 1024

// ErrClosedEarly indicates the provider closed its stream channel
// without first emitting a terminal Done event. This is distinct
// from ctx cancellation (the caller's existing ctx.Err() check
// handles that) and signals a provider-side failure — network drop,
// SSE parse error, scanner overflow — that must not be mistaken for
// a clean completion. Silently accepting the partial content would
// let a truncated turn end in AgentWaiting with no error shown to
// the developer.
var ErrClosedEarly = errors.New("agent: provider closed stream before completion")

// Drain reads ch to completion, forwarding text tokens to send and
// collecting the terminal Done event. thinkState is mutated in place
// so <think>...</think> spans that cross chunk boundaries are
// stripped correctly across calls within a single turn.
//
// Returns [ErrClosedEarly] when the channel closes without a Done
// event and ctx is still live; the caller must treat this as a
// failed turn rather than a clean completion with partial content.
// A ctx cancellation returns (Result, nil) since the caller's
// existing ctx.Err() check handles it.
func Drain(ctx context.Context, ch <-chan llm.StreamEvent, thinkState *bool, send Sender) (Result, error) {
	// Each provider response is a self-contained turn — chat/
	// completion APIs don't carry <think> state across responses.
	// If this stream ended with an unclosed <think> block (truncation,
	// early EOF, ctx cancel), the next call would start with
	// inThink=true and silently drop real output waiting for a
	// </think> that will never come. Force-reset on every return path.
	defer func() { *thinkState = false }()

	var (
		buf    strings.Builder
		out    Result
		capped bool
	)
	for {
		select {
		case <-ctx.Done():
			out.Content = buf.String()
			return out, nil
		case ev, ok := <-ch:
			if !ok {
				out.Content = buf.String()
				if ctx.Err() != nil {
					return out, nil
				}
				return out, ErrClosedEarly
			}
			if ev.Done {
				out.ToolCalls = ev.ToolCalls
				out.Usage = ev.Usage
				out.Truncated = ev.Truncated
				out.Content = buf.String()
				return out, nil
			}
			if capped {
				continue // keep draining so the provider goroutine can exit cleanly
			}
			clean := stripThinkTags(ev.Token, thinkState)
			if clean == "" {
				continue
			}
			if buf.Len()+len(clean) > MaxContentBytes {
				slog.Warn("agent: content buffer cap reached, dropping subsequent tokens",
					"cap_bytes", MaxContentBytes)
				capped = true
				continue
			}
			buf.WriteString(clean)
			send(event.AgentToken{Text: clean})
		}
	}
}

// stripThinkTags removes <think>...</think> content from a string.
// inThink tracks state across calls for multi-line think blocks.
// Private to the package because the only legitimate caller is
// [Drain]; exporting it would invite per-token use outside the
// stream-consumer's invariants (notably the post-call reset).
func stripThinkTags(s string, inThink *bool) string {
	var out strings.Builder
	for len(s) > 0 {
		if *inThink {
			end := strings.Index(s, "</think>")
			if end == -1 {
				return out.String()
			}
			s = s[end+len("</think>"):]
			*inThink = false
		} else {
			start := strings.Index(s, "<think>")
			if start == -1 {
				out.WriteString(s)
				return out.String()
			}
			out.WriteString(s[:start])
			s = s[start+len("<think>"):]
			*inThink = true
		}
	}
	return out.String()
}
