package llm

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// CompactMessages returns a copy of messages with old tool results truncated
// to reduce token count. It preserves the system message (messages[0]) and
// the last keepTurns user-initiated exchanges intact. Tool results in the
// compactable region whose content exceeds minBytes are replaced with a
// short stub showing the tool name and original size.
//
// A "turn" is defined as a user message and everything that follows until
// the next user message. keepTurns=2 means the last two user messages and
// all subsequent assistant/tool messages are kept verbatim.
//
// Returns the (possibly compacted) message slice and a bool indicating
// whether any content was actually truncated. When false, the returned
// slice is the original unmodified input.
func CompactMessages(messages []Message, keepTurns, minBytes int) ([]Message, bool) {
	if len(messages) < 3 || keepTurns < 1 {
		return messages, false
	}

	// Find user message indices (skip system message at 0).
	var userIndices []int
	for i := 1; i < len(messages); i++ {
		if messages[i].Role == "user" {
			userIndices = append(userIndices, i)
		}
	}

	// Not enough turns to compact — keep everything.
	if len(userIndices) <= keepTurns {
		return messages, false
	}

	// The keep boundary: everything from this index onward is preserved.
	keepFrom := userIndices[len(userIndices)-keepTurns]

	// Build a tool-call-ID → tool-name map from assistant messages in the
	// compactable region so we can label truncated results.
	toolNames := buildToolNameMap(messages[1:keepFrom])

	// Copy messages, truncating old tool results.
	result := make([]Message, len(messages))
	result[0] = messages[0] // system prompt — never touch
	compacted := false
	for i := 1; i < len(messages); i++ {
		if i >= keepFrom {
			result[i] = messages[i]
			continue
		}
		m := messages[i]
		if m.Role == "tool" && len(m.Content) > minBytes {
			name := toolNames[m.ToolCallID]
			m = Message{
				Role:       "tool",
				ToolCallID: m.ToolCallID,
				Content:    truncateToolResult(name, m.Content),
			}
			compacted = true
		}
		result[i] = m
	}

	if !compacted {
		return messages, false
	}
	return result, true
}

// buildToolNameMap extracts tool call IDs and their function names from
// assistant messages in a slice. Used to label truncated tool results.
func buildToolNameMap(messages []Message) map[string]string {
	m := make(map[string]string)
	for _, msg := range messages {
		for _, tc := range msg.ToolCalls {
			m[tc.ID] = tc.Function.Name
		}
	}
	return m
}

// truncateToolResult builds a compact stub for a truncated tool result.
// Keeps the first line of content for context (e.g., a file path header
// or command output start), then appends a size note.
func truncateToolResult(toolName, content string) string {
	lines := strings.Count(content, "\n") + 1
	chars := utf8.RuneCountInString(content)

	var b strings.Builder
	if toolName != "" {
		fmt.Fprintf(&b, "[%s — ", toolName)
	} else {
		b.WriteString("[tool result — ")
	}
	fmt.Fprintf(&b, "%d lines, %d chars, truncated]\n", lines, chars)

	// Keep the first line as a preview when the content spans multiple lines
	// and the first line is short enough to be useful. Single-line content
	// that exceeds minBytes is typically a long blob (base64, minified JSON)
	// where a partial preview adds no value.
	if first, _, ok := strings.Cut(content, "\n"); ok && len(first) <= 200 {
		b.WriteString(first)
	}

	return b.String()
}
