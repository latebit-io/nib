package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
)

// ErrNothingToCompact is returned by [Agent.Compact] when the saved
// transcript is empty or compaction would not change it (no prunable
// tool results in the older turns). The caller should treat this as a
// no-op signal, not a failure.
var ErrNothingToCompact = errors.New("agent: nothing to compact")

// ErrNoConversation is returned by [Agent.Compact] and
// [Agent.ResetHistory] when there is no saved transcript to operate on.
var ErrNoConversation = errors.New("agent: no conversation")

// Compact forces a compaction pass over the saved transcript and
// installs the compacted slice as the next-resume seed. Cancels any
// active run first (callers must verify the agent is between turns —
// the registry's busy check is the contract surface). Emits
// [event.AgentCompacted] on success so the frontend can render the
// before/after token deltas.
//
// Returns [ErrNoConversation] when there is nothing saved,
// [ErrNothingToCompact] when compaction did not change the slice.
//
// Differs from the per-Stream [maybeCompact] hook in two ways:
//
//  1. Bypasses the [compactHistoryThreshold] gate — user-triggered
//     compaction runs on demand, not by token-budget policy.
//  2. Mutates the foundation's saved transcript directly via
//     [kit.Agent.ReplaceMessages] so the compaction is durable across
//     a Cancel + resume cycle.
func (a *Agent) Compact(ctx context.Context) error {
	if a.kit == nil {
		return ErrNoConversation
	}
	// Cheap pre-check on the live transcript so a no-conversation case
	// returns immediately instead of paying for cancelAndDrain. The
	// authoritative snapshot is taken below, post-drain.
	if len(a.kit.State().Messages) == 0 {
		return ErrNoConversation
	}
	// Drain BEFORE snapshotting. A snapshot taken before cancelAndDrain
	// can lose any messages an in-flight run appends between the
	// snapshot and the drain — ReplaceMessages would then install a
	// stale slice and silently discard those appends. The TUI's busy
	// check makes this race impossible in the standard wiring (Compact
	// is gated on !IsRunning() || IsWaiting()), but Compact is exported
	// and any direct caller deserves the safe ordering.
	if err := a.cancelAndDrain(ctx); err != nil {
		return err
	}
	saved := a.kit.State().Messages
	if len(saved) == 0 {
		return ErrNoConversation
	}
	a.mu.Lock()
	toolDefs := a.toolDefs
	a.mu.Unlock()
	before := llm.EstimateMessageTokens(saved, toolDefs)
	compacted, changed := llm.CompactMessages(saved, compactKeepTurns, compactMinBytes)
	if !changed {
		return ErrNothingToCompact
	}
	if err := a.kit.ReplaceMessages(compacted); err != nil {
		return fmt.Errorf("replace messages: %w", err)
	}
	after := llm.EstimateMessageTokens(compacted, toolDefs)
	a.send(event.AgentCompacted{
		BeforeTokens: before.History,
		AfterTokens:  after.History,
	})
	slog.Info("conversation compacted (manual)",
		"before", before.History,
		"after", after.History,
		"saved", before.History-after.History,
	)
	return nil
}

// ResetHistory drops the saved transcript so the next user message
// starts a fresh conversation. Cancels any active run first.
//
// Foundation state cleared:
//   - Saved transcript (via [kit.Agent.ReplaceMessages] with nil).
//
// Coding-side state cleared:
//   - Active intent and the trailing per-turn task/lint state, so a
//     subsequent goal does not inherit context from the previous run.
//
// The TUI's transcript view is the frontend's responsibility — this
// method does not touch it. Callers (typically /clear in the TUI
// command layer) are expected to clear their own view alongside this
// call.
func (a *Agent) ResetHistory(ctx context.Context) error {
	if a.kit == nil {
		return ErrNoConversation
	}
	if err := a.cancelAndDrain(ctx); err != nil {
		return err
	}
	if err := a.kit.ReplaceMessages(nil); err != nil {
		return fmt.Errorf("replace messages: %w", err)
	}
	a.mu.Lock()
	a.intent = ""
	a.taskEdits = nil
	a.pendingLint = ""
	a.runUnsuccessful = false
	a.budgetExceeded = false
	a.mu.Unlock()
	slog.Info("conversation history reset")
	return nil
}

// cancelAndDrain cancels the active run (if any), waits for the
// foundation goroutine to unwind, and fences the forwarder so any
// AgentDone-bearing event has been dispatched before the caller
// proceeds. Used by [Agent.Compact] and [Agent.ResetHistory] to reach
// a quiescent state where the foundation's transcript can be safely
// replaced.
//
// Idempotent: a no-op when no run is active. ctx is honored on the
// implicit forwarder fence indirectly — fenceForwarder waits on a
// channel close and is fast at this stage of the run, so a deadline
// is not threaded through.
func (a *Agent) cancelAndDrain(_ context.Context) error {
	if !a.IsRunning() {
		return nil
	}
	a.Cancel()
	a.kit.WaitForIdle()
	a.fenceForwarder()
	return nil
}

// compactHistoryThreshold is the estimated history token count above
// which old tool results are truncated to reduce input cost.
// Compaction is triggered before each LLM call so the next request
// fits a smaller window without losing the recent conversation.
//
// Set to 15k after a real session at 30k showed history climbing to
// 44k+ with only 36% cache hit rate on Codex — meaning ~28k tokens
// of history was being re-billed every turn at full rate. Tightening
// the threshold trades a more aggressive summarization pass against
// per-turn input cost; with the Codex pool's bipolar cache behavior
// (some turns hit 90%+, others 5–20%), reducing the size of the part
// that can miss is the dominant cost lever.
const compactHistoryThreshold = 15_000

// compactKeepTurns is the number of recent user turns whose tool
// results are preserved verbatim during compaction. Older tool
// results are summarised so the model still sees the conversation
// shape but not its full historical detail.
const compactKeepTurns = 3

// compactMinBytes is the minimum tool result size (bytes) below
// which compaction leaves the result untouched. Smaller results
// cost little to keep and pruning them yields negligible savings.
const compactMinBytes = 200

// maybeCompact checks whether the conversation history is large
// enough to warrant compaction and runs the two-tier compaction
// pipeline when it is. Tier 1 is the cheap tool-result truncation
// inherited from earlier work; Tier 2 is the LLM-backed summarization
// added in commit 3 of the agent-token-efficiency plan. Returns the
// (possibly compacted) message slice.
//
//   - Tier 1 fires when estimated history meets [compactHistoryThreshold].
//     It delegates to [llm.CompactMessages] which stubs old tool
//     results larger than [compactMinBytes]. Q&A-heavy sessions and
//     small-result tool chains often hit the threshold without
//     anything Tier 1 can prune — that's the misfire warn log surfaces.
//
//   - Tier 2 fires when post-Tier-1 history still meets
//     [summarizationThreshold] AND a viable user-boundary split exists
//     in the slice. It runs one non-streaming LLM call against the
//     agent's raw provider to replace the older range with a single
//     summary message. Failure (provider error, empty response, ctx
//     cancel) is a soft degrade: the post-Tier-1 slice is returned
//     unchanged. Tier 2 emits [event.AgentCompactionSummary] on
//     success; Tier 1 emits [event.AgentCompacted].
//
// Called from the foundation's TransformContext hook before each LLM
// call so the request sees a compacted slice without callers needing
// to know compaction happened. ctx threads through so the
// summarization LLM call inherits the run's cancellation.
func (a *Agent) maybeCompact(ctx context.Context, messages []llm.Message, toolDefs []llm.ToolDef) []llm.Message {
	est := llm.EstimateMessageTokens(messages, toolDefs)
	logCompactEvaluation(est.History, messages)

	if est.History < compactHistoryThreshold {
		return messages
	}

	// Tier 1 — truncate old tool results.
	tier1Out, tier1Changed := llm.CompactMessages(messages, compactKeepTurns, compactMinBytes)
	tier1Est := est
	if tier1Changed {
		tier1Est = llm.EstimateMessageTokens(tier1Out, toolDefs)
		a.send(event.AgentCompacted{
			BeforeTokens: est.History,
			AfterTokens:  tier1Est.History,
		})
		slog.Info("conversation compacted (tier 1)",
			"before", est.History,
			"after", tier1Est.History,
			"saved", est.History-tier1Est.History,
		)
		messages = tier1Out
	} else {
		slog.Warn("compact: threshold crossed but tier 1 made no change",
			"history", est.History,
			"threshold", compactHistoryThreshold,
			"messages", len(messages),
			"keep_turns", compactKeepTurns,
			"min_bytes", compactMinBytes)
	}

	// Tier 2 — summarize older messages if Tier 1 alone wasn't enough.
	if tier1Est.History < summarizationThreshold {
		return messages
	}
	if a.provider == nil {
		// Test fixtures may construct an Agent literal without a
		// provider; degrade silently rather than dereferencing nil.
		return messages
	}
	tier2Out, summary, summarized, ok := a.runTier2(ctx, messages)
	if !ok {
		return messages
	}
	tier2Est := llm.EstimateMessageTokens(tier2Out, toolDefs)
	a.send(event.AgentCompactionSummary{
		BeforeTokens:       tier1Est.History,
		AfterTokens:        tier2Est.History,
		SummarizedMessages: summarized,
		Summary:            summary,
	})
	slog.Info("conversation compacted (tier 2 — summarized)",
		"before", tier1Est.History,
		"after", tier2Est.History,
		"saved", tier1Est.History-tier2Est.History,
		"summarized_messages", summarized,
	)
	return tier2Out
}

// runTier2 performs the summarization-tier pass. Returns (out, summary,
// summarizedCount, true) on success; (nil, "", 0, false) on soft
// degrade (nothing to summarize, summarization failed, ctx cancelled).
//
// Soft-degrade contract: any non-fatal condition logs at warn or info
// and returns ok=false. The caller continues with the pre-Tier-2
// slice. Tier 2 must never panic, must never propagate an error to
// the foundation hook (the developer's run does not fail because
// compaction failed), and must release the provider snapshot before
// returning.
func (a *Agent) runTier2(ctx context.Context, messages []llm.Message) (out []llm.Message, summary string, summarized int, ok bool) {
	splitIdx, err := splitForSummarization(messages)
	if err != nil {
		// Not enough history with a clean user-boundary cut — let the
		// next turn try again as the slice grows.
		slog.Info("compact: tier 2 skipped — no viable split",
			"messages", len(messages),
			"err", err)
		return nil, "", 0, false
	}

	// Snapshot the provider under lock so a concurrent SetProvider
	// can't swap it mid-call. The summarization call uses the raw
	// provider (not the proxy) so its tokens don't pollute session
	// budget accounting.
	a.mu.Lock()
	provider := a.provider
	a.mu.Unlock()

	transcript := renderTranscript(messages[1:splitIdx])
	summary, err = runSummarization(ctx, provider, transcript)
	if err != nil {
		slog.Warn("compact: tier 2 summarization failed; keeping post-tier-1 slice",
			"err", err,
			"split_idx", splitIdx,
			"range_messages", splitIdx-1)
		return nil, "", 0, false
	}

	// Build the new slice: system prompt + summary + recent verbatim tail.
	out = make([]llm.Message, 0, 2+(len(messages)-splitIdx))
	out = append(out, messages[0])
	out = append(out, llm.Message{
		Role:    "assistant",
		Content: summaryPrefix + "\n\n" + summary,
	})
	out = append(out, messages[splitIdx:]...)
	return out, summary, splitIdx - 1, true
}

// logCompactEvaluation emits the per-turn diagnostic that surfaces
// whether Tier 1 has anything to prune (`tool_results_above_min`) or
// is silently no-oping on a Q&A-heavy session. Kept on the cheap path
// so /context callers always see a fresh snapshot in the log alongside
// their inspection.
func logCompactEvaluation(history int, messages []llm.Message) {
	roleCounts := make(map[string]int, 5)
	var bigToolResults, smallToolResults int
	for _, m := range messages {
		roleCounts[m.Role]++
		if m.Role == "tool" {
			if len(m.Content) > compactMinBytes {
				bigToolResults++
			} else {
				smallToolResults++
			}
		}
	}
	slog.Debug("compact: evaluate",
		"history", history,
		"tier1_threshold", compactHistoryThreshold,
		"tier2_threshold", summarizationThreshold,
		"messages", len(messages),
		"roles", roleCounts,
		"tool_results_above_min", bigToolResults,
		"tool_results_below_min", smallToolResults)
}

// AgentInputEstimate emission lives on [providerProxy.Stream] —
// computed from the exact (messages, tools) the inner provider
// receives, emitted synchronously before kicking off the LLM call.
// A previous design lived in coding's TransformContext but the
// pairing with the matching post-Stream AgentTurnUsage required
// reliable kit delivery, which kit explicitly does not guarantee
// for streaming events.
