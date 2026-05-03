package agent

import (
	"log/slog"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
)

// compactHistoryThreshold is the estimated history token count above
// which old tool results are truncated to reduce input cost.
// Compaction is triggered before each LLM call so the next request
// fits a smaller window without losing the recent conversation.
const compactHistoryThreshold = 30_000

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
