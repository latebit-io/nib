// drain.go + compact.go own the LLM I/O cost-control pipeline:
// consuming the provider's stream channel into a single turn's
// [streamResult], and pruning conversation history before it grows
// past [compactHistoryThreshold]. Both transform values and emit
// events through the agent's [sender]; they own no run state, no
// goroutine, and no channels of their own.
//
// drain.go owns the per-stream consumer: token forwarding,
// <think>...</think> stripping, byte-cap enforcement, terminal-event
// detection. compact.go owns the per-turn history pruner: token
// estimation against [compactHistoryThreshold] and dispatch into
// [llm.CompactMessages].
package agent

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
)

// sender is the agent's event emitter shape. [drainStream] forwards
// stream tokens through it and the compaction helpers surface their
// own status events the same way. Defined as a func type so callers
// can pass a method value (e.g. `a.send`) without an adapter.
type sender func(event.Event)

// streamResult aggregates the outcome of one LLM streaming response
// after the terminal Done event is observed. Tokens are forwarded to
// the [sender] as they arrive; only the final accumulated state flows
// back to the caller via this struct.
type streamResult struct {
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
	// path in the run loop; see [recoverFromTruncation].
	Truncated bool
}

// maxStreamContentBytes caps the text content accumulated from a single
// streaming response. Matches the SSE scanner and tool-arg caps used
// elsewhere in the LLM layer. A misbehaving provider (e.g. one stuck
// in a regeneration loop) cannot exhaust memory by streaming
// unbounded tokens; once the cap is reached, subsequent tokens are
// dropped and the overflow is logged.
const maxStreamContentBytes = 10 * 1024 * 1024

// errStreamClosedEarly indicates the provider closed its stream channel
// without first emitting a terminal Done event. This is distinct
// from ctx cancellation (the caller's existing ctx.Err() check
// handles that) and signals a provider-side failure — network drop,
// SSE parse error, scanner overflow — that must not be mistaken for
// a clean completion. Silently accepting the partial content would
// let a truncated turn end in AgentWaiting with no error shown to
// the developer.
var errStreamClosedEarly = errors.New("agent: provider closed stream before completion")

// drainStream reads ch to completion, forwarding text tokens to send
// and collecting the terminal Done event. thinkState is mutated in
// place so <think>...</think> spans that cross chunk boundaries are
// stripped correctly across calls within a single turn.
//
// drainStream also handles the cross-token-boundary case: a tag split
// across StreamEvent.Token chunks (e.g. "Hello <thi" then
// "nk>private</think>visible") is stitched back together by holding
// the trailing partial-tag bytes as residue and re-scanning when the
// next token arrives. Without this the leading "<thi" would have been
// emitted verbatim and the matching "nk>private</think>" would have
// leaked the entire think block to the sender.
//
// Returns [errStreamClosedEarly] when the channel closes without a
// Done event and ctx is still live; the caller must treat this as a
// failed turn rather than a clean completion with partial content. A
// ctx cancellation returns (streamResult, nil) since the caller's
// existing ctx.Err() check handles it.
func drainStream(ctx context.Context, ch <-chan llm.StreamEvent, thinkState *bool, send sender) (streamResult, error) {
	// Each provider response is a self-contained turn — chat/
	// completion APIs don't carry <think> state across responses.
	// If this stream ended with an unclosed <think> block (truncation,
	// early EOF, ctx cancel), the next call would start with
	// inThink=true and silently drop real output waiting for a
	// </think> that will never come. Force-reset on every return path.
	defer func() { *thinkState = false }()

	var (
		buf     strings.Builder
		out     streamResult
		capped  bool
		residue string // trailing partial-tag bytes held for the next token
	)
	emit := func(s string) bool {
		if s == "" {
			return false
		}
		if buf.Len()+len(s) > maxStreamContentBytes {
			slog.Warn("agent: content buffer cap reached, dropping subsequent tokens",
				"cap_bytes", maxStreamContentBytes)
			return true
		}
		buf.WriteString(s)
		send(event.AgentToken{Text: s})
		return false
	}
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
				return out, errStreamClosedEarly
			}
			if ev.Done {
				// Stream ended with a residue still held: flush it as
				// content if we are NOT inside a think block. A residue
				// in !inThink is "looked-like-a-tag-opener but never
				// materialized" — original behavior was to ship those
				// bytes verbatim. A residue in inThink is "looked-like-
				// a-tag-closer inside a think span" — we suppress it
				// because we never exited the think block.
				if !*thinkState && residue != "" && !capped {
					emit(residue)
				}
				out.ToolCalls = ev.ToolCalls
				out.Usage = ev.Usage
				out.Truncated = ev.Truncated
				out.Content = buf.String()
				return out, nil
			}
			if capped {
				continue // keep draining so the provider goroutine can exit cleanly
			}
			var clean string
			clean, residue = stripThinkTags(residue+ev.Token, thinkState)
			capped = emit(clean)
		}
	}
}

// stripThinkTags removes <think>...</think> content from s. inThink
// tracks state across calls for multi-line think blocks. The returned
// residue is any trailing portion of s that COULD be the start of a
// tag boundary the next call must complete — e.g. an input ending in
// "<thi" returns clean="" and residue="<thi", so the caller can
// prepend it to the next token and detect a "<think>" that spans
// the chunk boundary.
//
// Private to the file because the only legitimate caller is
// [drainStream]; exposing it would invite per-token use outside the
// stream-consumer's invariants (notably the post-call reset and the
// residue threading).
func stripThinkTags(s string, inThink *bool) (clean, residue string) {
	var out strings.Builder
	for len(s) > 0 {
		if *inThink {
			end := strings.Index(s, "</think>")
			if end == -1 {
				// Inside a think block; suppress everything but hold
				// any trailing prefix of "</think>" so the next call
				// can complete a closer that spans the boundary.
				held := trailingTagPrefix(s, "</think>")
				return out.String(), s[len(s)-held:]
			}
			s = s[end+len("</think>"):]
			*inThink = false
		} else {
			start := strings.Index(s, "<think>")
			if start == -1 {
				// No opener; emit safe content and hold any trailing
				// prefix of "<think>" so the next call can complete
				// an opener that spans the boundary. The held bytes
				// are NOT emitted yet — if they don't materialize,
				// they'll be flushed on stream-end (see [drainStream]).
				held := trailingTagPrefix(s, "<think>")
				out.WriteString(s[:len(s)-held])
				return out.String(), s[len(s)-held:]
			}
			out.WriteString(s[:start])
			s = s[start+len("<think>"):]
			*inThink = true
		}
	}
	return out.String(), ""
}

// trailingTagPrefix returns the length of the longest suffix of s
// that is also a prefix of pattern. Used to detect a tag whose head
// landed at the end of one chunk and whose tail is in the next.
//
// Capped at len(pattern)-1: a complete match would have been found
// by [strings.Index] before this helper is called. Empty pattern or
// empty s returns 0. Pure: no allocations beyond the slice header.
func trailingTagPrefix(s, pattern string) int {
	maxCheck := len(pattern) - 1
	if len(s) < maxCheck {
		maxCheck = len(s)
	}
	for n := maxCheck; n > 0; n-- {
		if s[len(s)-n:] == pattern[:n] {
			return n
		}
	}
	return 0
}
