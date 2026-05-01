package streaming

import (
	"log/slog"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/engine/event"
)

// CompactHistoryThreshold is the estimated history token count above
// which old tool results are truncated to reduce input cost.
// Compaction is triggered before each LLM call so the next request
// fits a smaller window without losing the recent conversation.
const CompactHistoryThreshold = 30_000

// CompactKeepTurns is the number of recent user turns whose tool
// results are preserved verbatim during compaction. Older tool
// results are summarised so the model still sees the conversation
// shape but not its full historical detail.
const CompactKeepTurns = 3

// CompactMinBytes is the minimum tool result size (bytes) below
// which compaction leaves the result untouched. Smaller results
// cost little to keep and pruning them yields negligible savings.
const CompactMinBytes = 200

// MaybeCompact checks whether the conversation history is large
// enough to warrant compaction. If so, truncates old tool results
// (delegating to [llm.CompactMessages]) and emits an AgentCompacted
// event. Returns the (possibly compacted) message slice.
//
// The decision is based on [llm.EstimateMessageTokens] against
// [CompactHistoryThreshold]; estimation runs on every turn but
// compaction itself only fires when the threshold is crossed AND
// [llm.CompactMessages] reports that something actually changed
// (a tree of small tool results may estimate over the threshold
// but contain nothing prunable).
func MaybeCompact(messages []llm.Message, toolDefs []llm.ToolDef, send Sender) []llm.Message {
	est := llm.EstimateMessageTokens(messages, toolDefs)
	if est.History < CompactHistoryThreshold {
		return messages
	}
	compacted, changed := llm.CompactMessages(messages, CompactKeepTurns, CompactMinBytes)
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

// EstimateAndBroadcast computes a client-side input estimate and
// emits an AgentInputEstimate event so the frontend status bar
// updates before the LLM call starts. Returns the same estimate so
// the caller can store it for budget bookkeeping without re-
// computing.
func EstimateAndBroadcast(messages []llm.Message, toolDefs []llm.ToolDef, send Sender) llm.InputEstimate {
	est := llm.EstimateMessageTokens(messages, toolDefs)
	send(event.AgentInputEstimate{
		System:  est.System,
		Tools:   est.Tools,
		History: est.History,
		New:     est.New,
	})
	return est
}
