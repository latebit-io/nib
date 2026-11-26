package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProjectInstructions_ReachThePrompt: an AGENTS.md at the root is
// appended to the system prompt, after the base prompt.
func TestProjectInstructions_ReachThePrompt(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("Run make test before finishing."), 0o644); err != nil {
		t.Fatal(err)
	}
	files := loadContextFiles(root)
	prompt := withProjectInstructions(buildSystemPrompt("s1"), files)
	if !strings.HasPrefix(prompt, buildSystemPrompt("s1")) {
		t.Error("the base prompt must come first, unchanged")
	}
	if !strings.Contains(prompt, "Run make test before finishing.") || !strings.Contains(prompt, "<project_instructions") {
		t.Errorf("the prompt lacks the framed AGENTS.md:\n%s", prompt)
	}
}

func TestProjectInstructions_NoneLeavesPromptAlone(t *testing.T) {
	files := loadContextFiles(t.TempDir())
	if got, want := withProjectInstructions("base", files), "base"; got != want {
		t.Errorf("prompt = %q, want %q", got, want)
	}
}
