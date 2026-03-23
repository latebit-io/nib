package agent

import (
	"fmt"
	"strings"

	"github.com/latebit-io/junto/engine/llm"
)

// maxContextInPrompt caps how many context files are listed in the prompt.
// Prevents unbounded prompt growth in long-lived sessions.
const maxContextInPrompt = 50

// buildMessages constructs the message list for an LLM request.
// fileContent is the raw file contents; this function will prepend 1-indexed line numbers.
// contextFiles lists the files the agent is allowed to edit.
func (a *Agent) buildMessages(fileName, fileContent, goal string, contextFiles []string) []llm.Message {
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

	// Cap context files for prompt size
	shown := contextFiles
	omitted := 0
	if len(shown) > maxContextInPrompt {
		omitted = len(shown) - maxContextInPrompt
		shown = shown[:maxContextInPrompt]
	}

	userContent, err := a.prompts.RenderUserMessage(UserPromptData{
		FileName:     fileName,
		FileContent:  numbered.String(),
		Fence:        fence,
		ContextFiles: shown,
		OmittedCount: omitted,
		Goal:         goal,
	})
	if err != nil {
		// Template execution failed — fall back to a minimal message.
		// This should not happen with the embedded default template,
		// but a broken project override could trigger it.
		userContent = fmt.Sprintf("## File: %s\n\n## Task\n\n%s", fileName, goal)
	}

	return []llm.Message{
		{Role: "system", Content: a.prompts.SystemPrompt()},
		{Role: "user", Content: userContent},
	}
}
