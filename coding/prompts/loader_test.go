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
