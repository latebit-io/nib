package prompts

import (
	"strings"
	"testing"
)

// TestSystemPrompt_ProjectMDIsMemoryResident locks the clarification that
// the work tree lives in memory, not on disk. Without it the model reads
// the "/project.md" path as a filesystem path and tries to read_file a
// nonexistent local plan before falling back to memory.
func TestSystemPrompt_ProjectMDIsMemoryResident(t *testing.T) {
	got := NewPromptLoader("").SystemPrompt(SystemPromptData{})

	for _, want := range []string{
		"memory_fetch /project.md",
		"not on disk",
		"Never `read_file` it or make a local copy",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("system prompt missing %q; the local-plan regression is unguarded", want)
		}
	}
}

func TestSystemPrompt_AgentPersona(t *testing.T) {
	l := NewPromptLoader("")
	const persona = "You are a meticulous security reviewer. Focus on injection flaws."

	// Empty persona: the prompt is byte-identical to a no-data render, so
	// the top-level agent is unaffected.
	base := l.SystemPrompt(SystemPromptData{})
	if strings.Contains(base, "specialized subagent") {
		t.Fatal("base prompt should not contain the persona section")
	}

	// Persona set: injected verbatim, above the operational scaffolding.
	withPersona := l.SystemPrompt(SystemPromptData{AgentPersona: persona})
	if !strings.Contains(withPersona, persona) {
		t.Error("persona text missing from system prompt")
	}
	if !strings.Contains(withPersona, "specialized subagent") {
		t.Error("persona section marker missing")
	}
	// Augment, not replace: the base operational content is still present.
	if !strings.Contains(withPersona, "memory_fetch /project.md") {
		t.Error("persona render dropped the base operational scaffolding")
	}
	if !strings.HasPrefix(strings.TrimSpace(withPersona), "You are operating as a specialized subagent") {
		t.Error("persona should lead the system prompt (highest salience)")
	}

	// Planning mode carries the persona too.
	plan := l.PlanningSystemPrompt(SystemPromptData{AgentPersona: persona})
	if !strings.Contains(plan, persona) {
		t.Error("planning system prompt missing persona")
	}
}
