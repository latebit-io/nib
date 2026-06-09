package agent

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/coding/prompts"
	"github.com/latebit-io/nib/kit/skill"
)

// hasSkills reports whether any model-invoked skill tool is registered,
// detected by the [skill.ToolNamePrefix] convention. Drives the
// prompt's skills section without threading extra construction state.
func (a *Agent) hasSkills() bool {
	for _, def := range a.toolDefs {
		if strings.HasPrefix(def.Function.Name, skill.ToolNamePrefix) {
			return true
		}
	}
	return false
}

// maxMemorySummaryBytes caps the memory summary injected into the prompt.
// The summary is external content from demarkus — must be bounded.
const maxMemorySummaryBytes = 8000

// maxActiveTaskPathBytes caps the active task ancestry path in the prompt.
const maxActiveTaskPathBytes = 1024

// buildMessages constructs the message list for an LLM request.
// fileContent is the raw file contents; this function will prepend 1-indexed line numbers.
// memorySummary is the project memory snapshot (passed in to avoid shared state races).
func (a *Agent) buildMessages(fileName, fileContent, goal string, memorySummary string, mode event.Mode) []llm.Message {
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

	userContent, err := a.prompts.RenderUserMessage(prompts.UserPromptData{
		FileName:       fileName,
		Language:       prompts.DetectLanguage(fileName),
		FileContent:    numbered.String(),
		Fence:          fence,
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

	sysData := prompts.SystemPromptData{
		Headless:          a.interactionMode == Headless,
		DistributedMemory: a.distributedMemory,
		Terse:             a.currentTerse(),
		HasSkills:         a.hasSkills(),
	}
	systemPrompt := a.prompts.SystemPrompt(sysData)
	if mode == event.ModePlanning {
		systemPrompt = a.prompts.PlanningSystemPrompt(sysData)
	}

	return []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userContent},
	}
}

// rebuildSystemPrompt regenerates the system prompt text using the current
// runtime state (e.g. terse mode). Called between conversation turns so
// that runtime toggle changes take effect immediately without requiring
// a new RunWithMode call.
func (a *Agent) rebuildSystemPrompt(mode event.Mode) string {
	sysData := prompts.SystemPromptData{
		Headless:          a.interactionMode == Headless,
		DistributedMemory: a.distributedMemory,
		Terse:             a.currentTerse(),
		HasSkills:         a.hasSkills(),
	}
	if mode == event.ModePlanning {
		return a.prompts.PlanningSystemPrompt(sysData)
	}
	return a.prompts.SystemPrompt(sysData)
}
