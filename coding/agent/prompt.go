package agent

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/latebit-io/nib/ai/llm"
)

// maxContextInPrompt caps how many context files are listed in the prompt.
// Prevents unbounded prompt growth in long-lived sessions.
const maxContextInPrompt = 50

// maxMemorySummaryBytes caps the memory summary injected into the prompt.
// The summary is external content from demarkus — must be bounded.
const maxMemorySummaryBytes = 8000

// maxActiveTaskPathBytes caps the active task ancestry path in the prompt.
const maxActiveTaskPathBytes = 1024

// buildMessages constructs the message list for an LLM request.
// fileContent is the raw file contents; this function will prepend 1-indexed line numbers.
// contextFiles lists the files the agent is allowed to edit.
// memorySummary is the project memory snapshot (passed in to avoid shared state races).
func (a *Agent) buildMessages(fileName, fileContent, goal string, contextFiles []string, memorySummary string, mode Mode) []llm.Message {
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

	if len(memorySummary) > maxMemorySummaryBytes {
		// Truncate at a rune boundary to avoid splitting multi-byte UTF-8.
		cut := maxMemorySummaryBytes
		for cut > 0 && !utf8.RuneStart(memorySummary[cut]) {
			cut--
		}
		memorySummary = memorySummary[:cut] + "\n\n[truncated]"
	}

	// Include the active task path if the workspace supports task tracking.
	var activeTaskPath string
	if tt, ok := a.workspace.(TaskTracker); ok {
		activeTaskPath = tt.ActiveTaskPath()
		if len(activeTaskPath) > maxActiveTaskPathBytes {
			cut := maxActiveTaskPathBytes
			for cut > 0 && !utf8.RuneStart(activeTaskPath[cut]) {
				cut--
			}
			activeTaskPath = activeTaskPath[:cut] + "…"
		}
	}

	userContent, err := a.prompts.RenderUserMessage(UserPromptData{
		FileName:       fileName,
		Language:       DetectLanguage(fileName),
		FileContent:    numbered.String(),
		Fence:          fence,
		ContextFiles:   shown,
		OmittedCount:   omitted,
		Goal:           goal,
		MemorySummary:  memorySummary,
		ActiveTaskPath: activeTaskPath,
	})
	if err != nil {
		// Template execution failed — fall back to a minimal message.
		// This should not happen with the embedded default template,
		// but a broken project override could trigger it.
		userContent = fmt.Sprintf("## File: %s\n\n## Task\n\n%s", fileName, goal)
	}

	sysData := SystemPromptData{
		Headless:          a.interactionMode == Headless,
		Autonomous:        a.currentAutonomous(),
		DistributedMemory: a.distributedMemory,
		CodingStyle:       a.currentCodingStyle(),
		Terse:             a.currentTerse(),
	}
	systemPrompt := a.prompts.SystemPrompt(sysData)
	if mode == ModePlanning {
		systemPrompt = a.prompts.PlanningSystemPrompt(sysData)
	}

	return []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userContent},
	}
}

// rebuildSystemPrompt regenerates the system prompt text using the current
// runtime state (e.g. coding style). Called between conversation turns so
// that changes from SetCodingStyle take effect immediately without requiring
// a new RunWithMode call.
func (a *Agent) rebuildSystemPrompt(mode Mode) string {
	sysData := SystemPromptData{
		Headless:          a.interactionMode == Headless,
		Autonomous:        a.currentAutonomous(),
		DistributedMemory: a.distributedMemory,
		CodingStyle:       a.currentCodingStyle(),
		Terse:             a.currentTerse(),
	}
	if mode == ModePlanning {
		return a.prompts.PlanningSystemPrompt(sysData)
	}
	return a.prompts.SystemPrompt(sysData)
}
