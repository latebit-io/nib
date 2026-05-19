package agent

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/kit/budget"
)

// providerProxy delegates [llm.Provider.Stream] to whatever provider is
// active at call time. Used to plug [Agent.SetProvider]'s hot-swap into
// the kit/foundation [agent.Agent], which captures its provider once at
// construction time and has no way to swap it later.
//
// The proxy is the [llm.Provider] handed to [kit.New]. Stream-side
// reads share an [sync.RWMutex] read lock so concurrent turns do not
// contend; [providerProxy.Set] takes the write lock briefly when the
// developer toggles models in the TUI. A swap mid-Stream lets the in-
// flight call complete on the old provider — the local snapshot
// captured before the RUnlock is the one passed to Stream.
//
// The proxy ALSO owns the per-turn lifecycle: estimate computation,
// AgentInputEstimate emission, session-usage accumulation, and the
// authoritative AgentTurnUsage emission. All of these happen
// synchronously in the foundation goroutine (or its Stream wrapper
// goroutine), bypassing kit's translator/forwarder lossy boundary so:
//
//   - The foundation's pre-Stream budget check (next turn's
//     TransformContext) sees fresh totals — eliminates the race
//     where coding's async forwarder could let one extra Stream
//     call slip past the budget gate.
//   - Per-turn estimate pairing cannot desync from a kit-dropped
//     AgentTurnUsage. Kit's AgentTurnUsage is a streaming event
//     (drop on full channel); coding cannot rely on its delivery
//     for internal bookkeeping. The proxy's emission to [Agent.send]
//     uses coding's own send semantics (control-event blocking +
//     5s timeout for AgentTurnUsage's per-turn semantic).
type providerProxy struct {
	mu sync.RWMutex
	p  llm.Provider

	// emit forwards per-turn events (AgentInputEstimate,
	// AgentTurnUsage) to the wrapper's frontend channel. Bound at
	// construction by [Agent.buildKitAgent] to [Agent.send]. Nil
	// during tests that construct providerProxy directly without
	// going through [Agent.New].
	emit func(event.Event)

	// onTurnSettled fires after AgentTurnUsage emission. Bound at
	// construction by [Agent.buildKitAgent] to a closure that runs
	// the post-turn budget check + abort. Synchronous in the Stream
	// wrapper goroutine; abort marks kit's outcome unsuccess so the
	// next AgentDone surfaces with Success=false.
	onTurnSettled func()

	sessionMu sync.Mutex
	session   budget.Session
}

// newProviderProxy returns a proxy seeded with the initial provider.
// p must be non-nil; callers should validate before calling. emit and
// onTurnSettled may be nil for tests that do not exercise the
// per-turn lifecycle (the Stream wrapper checks before invoking).
func newProviderProxy(p llm.Provider, emit func(event.Event), onTurnSettled func()) *providerProxy {
	return &providerProxy{
		p:             p,
		emit:          emit,
		onTurnSettled: onTurnSettled,
	}
}

// Stream delegates to the live provider under the read lock and runs
// the per-turn lifecycle (estimate emission, content accumulation,
// usage recording, AgentTurnUsage emission, post-turn budget check)
// synchronously in the wrapper goroutine. By the time drainStream
// (foundation) observes Done, all per-turn state has been committed
// — the foundation cannot start the next TransformContext until
// drainStream returns.
func (pp *providerProxy) Stream(ctx context.Context, messages []llm.Message, tools []llm.ToolDef) (<-chan llm.StreamEvent, error) {
	pp.mu.RLock()
	p := pp.p
	pp.mu.RUnlock()

	// Pre-Stream estimate. Computed from the exact (messages, tools)
	// the inner provider will see, so the AgentInputEstimate matches
	// the LLM's actual input shape.
	est := llm.EstimateMessageTokens(messages, tools)
	if pp.emit != nil {
		pp.emit(event.AgentInputEstimate{
			System:  est.System,
			Tools:   est.Tools,
			History: est.History,
			New:     est.New,
		})
	}

	inner, err := p.Stream(ctx, messages, tools)
	if err != nil {
		return nil, err
	}

	out := make(chan llm.StreamEvent, cap(inner))
	go func() {
		defer close(out)
		var content strings.Builder
		for {
			// Observe ctx.Done so the wrapper exits when drainStream
			// returns early (foundation cancelled mid-turn). Without
			// this, an unread `out <- ev` would block forever — the
			// wrapper would pin both itself AND the inner provider
			// goroutine that feeds `inner`.
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-inner:
				if !ok {
					return
				}
				if ev.Token != "" {
					content.WriteString(ev.Token)
				}
				if ev.Done {
					// Record + emit BEFORE forwarding Done so drainStream's
					// observation of Done synchronizes with the per-turn
					// commit. Foundation cannot start the next TransformContext
					// until drainStream returns, so the next pre-Stream
					// budget check sees the just-committed totals.
					if ev.Usage != nil {
						pp.recordUsage(ev.Usage)
					}
					turn := pp.currentTurn()
					promptTok := usagePromptTokens(ev.Usage)
					cachedTok := usageCachedTokens(ev.Usage)
					completionTok := usageCompletionTokens(ev.Usage)
					if pp.emit != nil {
						pp.emit(event.AgentTurnUsage{
							Turn:             turn,
							PromptTokens:     promptTok,
							CompletionTokens: completionTok,
							CachedTokens:     cachedTok,
							ToolCalls:        len(ev.ToolCalls),
							SystemEst:        est.System,
							ToolsEst:         est.Tools,
							HistoryEst:       est.History,
							NewEst:           est.New,
							CompletionEst:    llm.EstimateTokens(content.String()),
						})
					}
					pp.logTurnUsage(turn, promptTok, cachedTok, completionTok, len(ev.ToolCalls))
					if pp.onTurnSettled != nil {
						pp.onTurnSettled()
					}
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// logTurnUsage emits one INFO line per turn carrying the
// provider-reported token counts AND the rolling session totals.
// Captured at INFO so an unmodified production binary writes a
// complete per-turn billing trace to debug.log — recoverable later
// via grep without any extra flags or env vars.
//
// The per-turn fields (prompt/cached/completion) are what the
// provider's billing surface charges for. uncached_input is derived
// (prompt - cached) so a downstream tally doesn't need to subtract
// itself. The session_* fields advance monotonically; the LAST log
// line of a run carries the final session totals — making
// apples-to-apples billing comparison across nib runs (and against
// opencode / pi) a single `grep` + `tail -1` away.
func (pp *providerProxy) logTurnUsage(turn, prompt, cached, completion, toolCalls int) {
	totals := pp.Snapshot()
	uncached := prompt - cached
	if uncached < 0 {
		// Defensive: providers that mis-report cached > prompt
		// would produce a negative value that's nonsensical for
		// downstream summation. Clamp + log so the anomaly is
		// visible in the trace rather than silently distorting it.
		slog.Warn("provider usage: cached > prompt — clamping uncached to 0",
			"turn", turn, "prompt", prompt, "cached", cached)
		uncached = 0
	}
	slog.Info("provider usage: turn settled",
		"turn", turn,
		"prompt_tokens", prompt,
		"cached_tokens", cached,
		"uncached_tokens", uncached,
		"completion_tokens", completion,
		"tool_calls", toolCalls,
		"session_prompt_tokens", totals.TotalPromptTokens,
		"session_cached_tokens", totals.TotalCachedTokens,
		"session_completion_tokens", totals.TotalCompletionTokens,
		"session_turns", totals.Turns,
	)
}

// usagePromptTokens / usageCompletionTokens / usageCachedTokens are
// nil-safe accessors. A provider that does not report usage on Done
// emits a nil Usage; we still emit AgentTurnUsage with zeros for the
// provider-reported fields so the cost panel records a turn boundary.
func usagePromptTokens(u *llm.Usage) int {
	if u == nil {
		return 0
	}
	return u.PromptTokens
}

func usageCompletionTokens(u *llm.Usage) int {
	if u == nil {
		return 0
	}
	return u.CompletionTokens
}

func usageCachedTokens(u *llm.Usage) int {
	if u == nil {
		return 0
	}
	return u.CachedTokens
}

// Set installs a new provider for subsequent Stream calls. Calls
// already in flight retain their original provider via the local
// captured before the RUnlock.
func (pp *providerProxy) Set(p llm.Provider) {
	pp.mu.Lock()
	pp.p = p
	pp.mu.Unlock()
}

// recordUsage accumulates one Stream's usage into the per-run session
// total. Called from the Stream wrapper goroutine on the inner
// stream's Done event.
func (pp *providerProxy) recordUsage(u *llm.Usage) {
	pp.sessionMu.Lock()
	pp.session.TotalPromptTokens += u.PromptTokens
	pp.session.TotalCompletionTokens += u.CompletionTokens
	pp.session.TotalCachedTokens += u.CachedTokens
	pp.session.Turns++
	pp.sessionMu.Unlock()
}

// currentTurn returns the 1-indexed turn number of the just-completed
// Stream call. Reads under sessionMu to align with [recordUsage]'s
// increment.
func (pp *providerProxy) currentTurn() int {
	pp.sessionMu.Lock()
	defer pp.sessionMu.Unlock()
	return pp.session.Turns
}

// Snapshot returns a copy of the current per-run session usage. Safe
// to call from any goroutine.
func (pp *providerProxy) Snapshot() budget.Session {
	pp.sessionMu.Lock()
	defer pp.sessionMu.Unlock()
	return pp.session
}

// ResetSession zeroes the per-run session counter. Called from
// [Agent.RunWithMode] and [Agent.Reply] resume path so each run
// starts with a fresh budget + turn counter.
func (pp *providerProxy) ResetSession() {
	pp.sessionMu.Lock()
	pp.session = budget.Session{}
	pp.sessionMu.Unlock()
}
