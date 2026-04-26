package agent

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/llm"
)

// Conversation streaming + compaction helpers for Agent.
//
// drainStream owns the provider-stream loop: token forwarding,
// <think>-tag stripping, the streamResult aggregation that hands
// control back to processLLMTurn. maybeCompact and
// estimateAndBroadcast manage the cost-control side of the same
// pipeline — input estimates flow to the frontend before each call,
// and history is truncated when the estimate crosses the compaction
// threshold.
//
// Methods stay attached to Agent because the run loop accesses them
// inline; the file boundary makes the streaming responsibility visible
// without changing the API.

const (
	// compactHistoryThreshold is the estimated history token count above
	// which old tool results are truncated to reduce input cost.
	compactHistoryThreshold = 30_000
	// compactKeepTurns is the number of recent user turns whose tool
	// results are preserved verbatim during compaction.
	compactKeepTurns = 3
	// compactMinBytes is the minimum tool result size (bytes) to truncate.
	// Smaller results are kept as-is since they cost little.
	compactMinBytes = 200
)

// maxStreamContentBytes caps the text content accumulated from a single
// streaming response. Matches the SSE scanner and tool-arg caps used
// elsewhere in the LLM layer. A misbehaving provider (e.g., one stuck in a
// regeneration loop) cannot exhaust memory by streaming unbounded tokens;
// once reached, subsequent tokens are dropped and logged.
const maxStreamContentBytes = 10 * 1024 * 1024

// errStreamClosedEarly indicates the provider closed its stream channel
// without first emitting a terminal Done event. This is distinct from ctx
// cancellation (handled by the caller's ctx.Err() check) and signals a
// provider-side failure — network drop, SSE parse error, scanner overflow —
// that must not be mistaken for a clean completion. Silently accepting the
// partial content would let a truncated turn end in AgentWaiting with no
// error shown to the developer.
var errStreamClosedEarly = errors.New("agent: provider closed stream before completion")

// streamResult aggregates the outcome of one LLM streaming response after
// the Done event is observed. Tokens are forwarded to the event channel as
// they arrive; only the final accumulated state flows back to the caller.
type streamResult struct {
	content   string
	toolCalls []llm.ToolCall
	usage     *llm.Usage
	truncated bool
}

// maybeCompact checks whether conversation history is large enough to
// warrant compaction. If so, truncates old tool results and emits an
// AgentCompacted event. Returns the (possibly compacted) message slice.
func (a *Agent) maybeCompact(messages []llm.Message, toolDefs []llm.ToolDef) []llm.Message {
	est := llm.EstimateMessageTokens(messages, toolDefs)
	if est.History < compactHistoryThreshold {
		return messages
	}
	compacted, changed := llm.CompactMessages(messages, compactKeepTurns, compactMinBytes)
	if !changed {
		return messages
	}
	afterEst := llm.EstimateMessageTokens(compacted, toolDefs)
	a.send(event.AgentCompacted{
		BeforeTokens: est.History,
		AfterTokens:  afterEst.History,
	})
	slog.Info("conversation compacted",
		"before", est.History,
		"after", afterEst.History,
		"saved", est.History-afterEst.History,
	)
	return compacted
}

// estimateAndBroadcast computes a client-side input estimate and sends it
// to the frontend so the status bar updates before the LLM call starts.
func (a *Agent) estimateAndBroadcast(messages []llm.Message, toolDefs []llm.ToolDef) llm.InputEstimate {
	est := llm.EstimateMessageTokens(messages, toolDefs)
	a.send(event.AgentInputEstimate{
		System:  est.System,
		Tools:   est.Tools,
		History: est.History,
		New:     est.New,
	})
	return est
}

// drainStream reads the provider stream to completion, forwarding text
// tokens to the frontend and collecting the terminal Done event. thinkState
// is mutated in place so <think>...</think> spans that cross chunk
// boundaries are stripped correctly.
//
// Returns errStreamClosedEarly when the channel closes without a Done event
// and ctx is still live; the caller must treat this as a failed turn rather
// than a clean completion with partial content. A ctx cancellation returns
// (result, nil) since the caller's existing ctx.Err() check handles it.
func (a *Agent) drainStream(ctx context.Context, ch <-chan llm.StreamEvent, thinkState *bool) (streamResult, error) {
	// Each provider response is a self-contained turn — chat/completion APIs
	// don't carry <think> state across responses. If this stream ended with
	// an unclosed <think> block (truncation, early EOF, ctx cancel), the
	// next call would start with inThink=true and silently drop real output
	// waiting for a </think> that will never come. Force-reset on every
	// return path.
	defer func() { *thinkState = false }()

	var (
		buf    strings.Builder
		out    streamResult
		capped bool
	)
	for {
		select {
		case <-ctx.Done():
			out.content = buf.String()
			return out, nil
		case ev, ok := <-ch:
			if !ok {
				out.content = buf.String()
				if ctx.Err() != nil {
					return out, nil
				}
				return out, errStreamClosedEarly
			}
			if ev.Done {
				out.toolCalls = ev.ToolCalls
				out.usage = ev.Usage
				out.truncated = ev.Truncated
				out.content = buf.String()
				return out, nil
			}
			if capped {
				continue // keep draining so the provider goroutine can exit cleanly
			}
			clean := stripThinkTags(ev.Token, thinkState)
			if clean == "" {
				continue
			}
			if buf.Len()+len(clean) > maxStreamContentBytes {
				slog.Warn("agent: content buffer cap reached, dropping subsequent tokens",
					"cap_bytes", maxStreamContentBytes)
				capped = true
				continue
			}
			buf.WriteString(clean)
			a.send(event.AgentToken{Text: clean})
		}
	}
}
