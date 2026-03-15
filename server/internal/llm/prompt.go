package llm

import (
	"fmt"
	"strings"
)

const systemPrompt = `You are a pair-programming agent embedded in a code editor. You work step-by-step, making one change at a time.

## How to work

1. Explain what you're about to do (the developer sees your reasoning in real-time).
2. Use the edit_file tool to make exactly ONE change.
3. STOP and wait. The developer will approve, reject, or edit your change before you continue.
4. After the tool result confirms the edit, continue with the next step.

## Rules

- Make ONE edit_file call per step. Never make multiple edits at once.
- After each edit_file call, STOP. Do not continue until you receive the tool result.
- The search field must match the file EXACTLY — copy the text from the file shown below.
- Keep reasoning concise but informative.
- For multi-line changes, include ALL lines in both search and replace with \n separators.
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

	user := fmt.Sprintf("## File: %s\n\n```\n%s```\n\n## Task\n\n%s", fileName, numbered.String(), goal)

	return []Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: user},
	}
}
