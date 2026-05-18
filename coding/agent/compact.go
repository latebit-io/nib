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
// 20_000 is a moderate setting: empirically a Pac-Man-class build
// peaks history around 60-70k tokens, so the threshold fires 2-3
// times across the run instead of once near the end at 30_000.
// [compactKeepTurns] still preserves the last 3 user turns verbatim,
// so the LLM never loses access to its immediate working set —
// compaction only prunes large tool results in older turns, which
// the model can re-read on demand if it genuinely needs them. The
// re-read cost is a single small bash/read_file turn; keeping the
// old result in every subsequent prefix costs the full blob every
// turn until the run ends.
const compactHistoryThreshold = 20_000

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
// enough to warrant compaction. If so, truncates old tool results
// (delegating to [llm.CompactMessages]) and emits an AgentCompacted
// event. Returns the (possibly compacted) message slice.
//
// The decision is based on [llm.EstimateMessageTokens] against
// [compactHistoryThreshold]; estimation runs on every turn but
// compaction itself only fires when the threshold is crossed AND
// [llm.CompactMessages] reports that something actually changed
// (a tree of small tool results may estimate over the threshold
// but contain nothing prunable).
func maybeCompact(messages []llm.Message, toolDefs []llm.ToolDef, send sender) []llm.Message {
	est := llm.EstimateMessageTokens(messages, toolDefs)
	if est.History < compactHistoryThreshold {
		return messages
	}
	compacted, changed := llm.CompactMessages(messages, compactKeepTurns, compactMinBytes)
	if !changed {
		return messages
	}
	afterEst := llm.EstimateMessageTokens(compacted, toolDefs)
	send(event.AgentCompacted{
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

// AgentInputEstimate emission lives on [providerProxy.Stream] —
// computed from the exact (messages, tools) the inner provider
// receives, emitted synchronously before kicking off the LLM call.
// A previous design lived in coding's TransformContext but the
// pairing with the matching post-Stream AgentTurnUsage required
// reliable kit delivery, which kit explicitly does not guarantee
// for streaming events.
