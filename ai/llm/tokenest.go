package llm

import "encoding/json"

// EstimateTokens returns a rough token count for a string.
// Uses ~4 characters per token heuristic — accurate within ~15% for
// English text and source code. Zero external dependencies.
func EstimateTokens(s string) int {
	return (len(s) + 3) / 4
}

// InputEstimate holds estimated token counts for each category of a
// chat completion request. All values are client-side approximations.
type InputEstimate struct {
	// System is the estimated system prompt tokens.
	System int
	// Tools is the estimated tool definition tokens.
	Tools int
	// History is the estimated tokens for all messages between the system
	// prompt and the new content (the conversation prefix that grows each turn).
	History int
	// New is the estimated tokens for the latest user/tool messages — the
	// content added since the last turn.
	New int
	// Total is the sum of all categories.
	Total int
}

// EstimateMessageTokens breaks down a chat completion request into
// estimated token counts per category. The categorization:
//   - messages[0] with role "system" → System
//   - tools (serialized) → Tools
//   - messages between system and new content → History
//   - trailing block of new messages (from the last assistant message onward) → New
func EstimateMessageTokens(messages []Message, tools []ToolDef) InputEstimate {
	var est InputEstimate

	// Tools — serialize to get a realistic size estimate.
	if len(tools) > 0 {
		if data, err := json.Marshal(tools); err == nil {
			est.Tools = EstimateTokens(string(data))
		}
	}

	if len(messages) == 0 {
		est.Total = est.Tools
		return est
	}

	// System prompt — always first message if role is "system".
	start := 0
	if messages[0].Role == "system" {
		est.System = estimateMessage(messages[0])
		start = 1
	}

	// Find the boundary between history and new content.
	// New content = the trailing run of messages after the last assistant message.
	// In a typical multi-turn conversation:
	//   [system, user, assistant, tool, tool, user] → new = [user] (last user msg)
	//   [system, user, assistant, tool, tool, assistant, tool, user] → new = [user]
	//   [system, user] → new = [user] (first turn, no history)
	newStart := findNewContentStart(messages, start)

	// History = everything between system and new content.
	for i := start; i < newStart; i++ {
		est.History += estimateMessage(messages[i])
	}

	// New content = everything from newStart onward.
	for i := newStart; i < len(messages); i++ {
		est.New += estimateMessage(messages[i])
	}

	est.Total = est.System + est.Tools + est.History + est.New
	return est
}

// findNewContentStart returns the index where "new content" begins.
// New content is the trailing block of messages that were added in the
// current turn — everything after the last assistant message in the array.
// If there's no assistant message, the first non-system message is new.
func findNewContentStart(messages []Message, start int) int {
	lastAssistant := -1
	for i := start; i < len(messages); i++ {
		if messages[i].Role == "assistant" {
			lastAssistant = i
		}
	}
	if lastAssistant < 0 {
		return start // no assistant yet — everything is new (first turn)
	}
	return lastAssistant + 1
}

// estimateMessage returns the estimated token count for a single message,
// including role overhead (~4 tokens per message for role/formatting).
func estimateMessage(m Message) int {
	tokens := 4 // per-message overhead (role, separators)
	tokens += EstimateTokens(m.Content)
	for _, tc := range m.ToolCalls {
		tokens += EstimateTokens(tc.Function.Name)
		tokens += EstimateTokens(tc.Function.Arguments)
	}
	return tokens
}
