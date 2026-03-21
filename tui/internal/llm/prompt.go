package llm

import (
	"fmt"
	"strings"
)

const systemPrompt = `You are a pair-programming agent in a code editor. Make one edit at a time.

## Workflow

1. Call read_file to see the exact file content.
2. Say ONE sentence about what you will change and why.
3. Call edit_file with the exact text from read_file in the search field.
4. STOP. Wait for the tool result before continuing.
5. The tool result includes the updated file. Use it for your next edit.

## Rules

- ONE sentence of explanation, then immediately call the tool. Do not analyze, review, or discuss the code at length.
- ONE edit_file call per step. Never batch multiple edits.
- The search field must EXACTLY match text from the file. Copy it character-for-character from read_file output.
- The file below is shown with line numbers for reference only. Line numbers (e.g., "   1 | ") are NOT part of the file. Never include them in search text. Use read_file to get the raw content.
- Do NOT repeat or summarize what you already said. Do NOT comment on the quality of previous edits.
- After a rejection, try a different approach immediately. Do not explain why the previous attempt was wrong.
`

// BuildMessages constructs the message list for an LLM request.
// fileContent is the raw file contents; this function will prepend 1-indexed line numbers.
func BuildMessages(fileName, fileContent, goal string) []Message {
	// Number the lines for the LLM
	lines := strings.Split(fileContent, "\n")
	var numbered strings.Builder
	for i, line := range lines {
		fmt.Fprintf(&numbered, "%4d | %s\n", i+1, line)
	}

	// Use a fence that doesn't appear in the file content
	fence := "```"
	for strings.Contains(numbered.String(), fence) {
		fence += "`"
	}
	user := fmt.Sprintf("## File: %s\n\n%s\n%s%s\n\n## Task\n\n%s", fileName, fence, numbered.String(), fence, goal)

	return []Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: user},
	}
}
