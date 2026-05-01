// Package nudges holds the pure heuristics the agent uses to detect
// model misbehavior between turns: outstanding-work narrative drift,
// permission-seeking yields in autonomous mode, and the planning-mode
// tool blocklist. The detection helpers and marker tables live here so
// they can be unit-tested without an Agent. The orchestration that
// reads agent state (workspace, autonomy, task tree) and decides
// whether to inject a synthetic user message stays on the Agent — see
// runLoop and tryInjectPostTurnNudge.
package nudges

import (
	"strings"

	"github.com/latebit-io/nib/ai/llm"
)

// OutstandingNudgeMessage is the synthetic user message the narrative
// gate injects when the model yields with text that enumerates
// outstanding work while the task tree is empty. Phrased as a developer
// instruction so the model treats it as high-priority guidance, not
// background context.
const OutstandingNudgeMessage = "Your last message enumerated work as 'still needed', 'not yet', or 'remaining' while the tracked task tree was empty. Either:\n" +
	"  (a) the items are real — add each one as a pending task via project_task_add and continue working, or\n" +
	"  (b) the items are out of scope — rewrite the summary without that language so the project state is consistent.\n" +
	"Do not yield again until the narrative and the task tree agree."

// PermissionNudgeMessage is the synthetic user message the autonomous-
// mode permission-question gate injects when the model yields with a
// question instead of acting. The developer set autonomy high
// precisely to avoid these prompts and will not answer; reprompting
// forces the model to decide and proceed (or surface a concrete
// blocker, which is a different shape than a chat question).
const PermissionNudgeMessage = "Your last message ended with a question or permission-seeking offer. Autonomy is high — the developer will not answer. " +
	"Make the call yourself: pick the option that serves the current task, document the choice in your one-sentence pre-tool explanation, and proceed with a tool call. " +
	"If you genuinely cannot proceed, state the concrete blocker (the specific tool error, missing dependency, or unsanctioned destructive op) and what you tried to fix it — do NOT phrase it as a question."

// outstandingWorkMarkers are case-insensitive substrings that flag a
// wrap-up message as enumerating uncompleted work. Conservative on
// purpose — false positives nudge the model harmlessly; false negatives
// let the original bug through. Each entry is a phrase, not a single
// word, to reduce hits on neutral prose ("not" alone is far too broad).
var outstandingWorkMarkers = []string{
	"still need", // covers "still need", "still needs", "still needed"
	"not yet",    // "not yet implemented", "not yet wired"
	"need implementation",
	"needs implementation",
	"yet to be",
	"remaining work",
	"work remaining",
	"outstanding work",  // "outstanding" alone is an adjective that fires
	"outstanding items", // on benign praise ("outstanding work — all done")
	"to be implemented",
	"to be done",
	"to do:",
	"todo:",
}

// ContainsOutstandingWorkMarker reports whether s contains any of the
// outstandingWorkMarkers phrases (case-insensitive).
func ContainsOutstandingWorkMarker(s string) bool {
	lower := strings.ToLower(s)
	for _, marker := range outstandingWorkMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// permissionSeekingMarkers are case-insensitive substrings that flag a
// yielded turn as asking the developer for permission instead of
// acting. Mirrors the [outstandingWorkMarkers] pattern: phrases (not
// single words) chosen to keep false-positive rate low. Each entry was
// observed in real autonomous-mode failure transcripts where the model
// ended a turn with a permission-seeking offer that the developer
// (with autonomy dial high) would not answer.
//
// Trailing-question detection (`?`) is a separate signal — handled in
// [ShouldNudgePermissionQuestion] alongside this list — because some
// permission-seeking phrasing has no question mark ("Let me know if
// you want me to continue.") and some questions are not permission-
// seeking (rhetorical "Is this what you wanted?" before continuing).
// The two signals are OR'd, with tool-call presence short-circuiting
// both.
var permissionSeekingMarkers = []string{
	"let me know",
	"if you want",
	"if you'd like",
	"if you would like",
	"want me to continue",
	"want me to proceed",
	"shall i continue",
	"shall i proceed",
	"shall i ",
	"do you want",
	"would you like",
	"would you prefer",
	"i can continue",
	"i'll continue if",
	"say keep going",
	"say continue",
	"say go",
	"just say",
	"give me the green light",
	"awaiting your",
	"await your",
	"pending your",
	"ready when you are",
	"happy to continue",
	"happy to proceed",
	"on your signal",
	"on your go",
}

// ContainsPermissionSeekingMarker reports whether s contains any of
// the [permissionSeekingMarkers] phrases under case-insensitive
// comparison. Whitespace and punctuation in s are not normalised — the
// marker set already uses lowercased phrase fragments that survive the
// strings.ToLower of typical assistant prose.
func ContainsPermissionSeekingMarker(s string) bool {
	lower := strings.ToLower(s)
	for _, marker := range permissionSeekingMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// ShouldNudgePermissionQuestion reports whether the most recent
// assistant message yielded with a question OR a permission-seeking
// offer, while making no tool call. Returns false when the last
// message has tool calls (any question accompanying a tool call is
// rhetorical, not a yield), when no assistant message exists, or when
// the trimmed content neither ends in `?` nor contains a
// [permissionSeekingMarkers] phrase. Used by the autonomous-mode
// permission gate in the agent run loop.
func ShouldNudgePermissionQuestion(messages []llm.Message) bool {
	last := LastAssistantMessage(messages)
	if last == nil {
		return false
	}
	if len(last.ToolCalls) > 0 {
		return false // tool call accompanies the text — model is acting
	}
	trimmed := strings.TrimSpace(last.Content)
	if trimmed == "" {
		return false
	}
	if strings.HasSuffix(trimmed, "?") {
		return true
	}
	return ContainsPermissionSeekingMarker(trimmed)
}

// LastAssistantContent returns the Content of the most recent
// assistant message in the slice, or "" when none exists. Used to scan
// the model's final wrap-up text for outstanding-work markers.
func LastAssistantContent(messages []llm.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			return messages[i].Content
		}
	}
	return ""
}

// LastAssistantMessage returns the most recent assistant message in
// the slice, or nil when none exists. Distinct from
// [LastAssistantContent] (which returns Content) because the
// permission-question gate also needs to inspect ToolCalls.
func LastAssistantMessage(messages []llm.Message) *llm.Message {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			return &messages[i]
		}
	}
	return nil
}
