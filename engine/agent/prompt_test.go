package agent

import (
	"os"
	"strings"
	"testing"
)

// testAgent returns a minimal agent with embedded prompts (no project overrides).
func testAgent() *Agent {
	return &Agent{
		prompts: NewPromptLoader(""),
	}
}

func TestBuildMessagesIncludesContextSet(t *testing.T) {
	a := testAgent()
	contextFiles := []string{"src/auth.go", "src/handler.go"}
	msgs := a.buildMessages("main.go", "package main", "add tests", contextFiles)

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	user := msgs[1].Content
	if !strings.Contains(user, "Context Set") {
		t.Error("user message should contain context set section")
	}
	if !strings.Contains(user, "- src/auth.go") {
		t.Error("user message should list src/auth.go")
	}
	if !strings.Contains(user, "- src/handler.go") {
		t.Error("user message should list src/handler.go")
	}
}

func TestBuildMessagesNoContextSet(t *testing.T) {
	a := testAgent()
	msgs := a.buildMessages("main.go", "package main", "add tests", nil)

	user := msgs[1].Content
	if strings.Contains(user, "Context Set") {
		t.Error("user message should not contain context set section when no context files")
	}
}

func TestBuildMessagesSystemPromptIncludesContextSetRules(t *testing.T) {
	a := testAgent()
	msgs := a.buildMessages("main.go", "package main", "add tests", nil)

	system := msgs[0].Content
	if !strings.Contains(system, "Context Set") {
		t.Error("system prompt should include context set section")
	}
}

func TestBuildMessagesSystemPromptIncludesCriticalPerspective(t *testing.T) {
	a := testAgent()
	msgs := a.buildMessages("main.go", "package main", "add tests", nil)

	system := msgs[0].Content
	if !strings.Contains(system, "Critical Perspective") {
		t.Error("system prompt should include critical perspective section")
	}
}

func TestBuildMessagesContextSetCapped(t *testing.T) {
	a := testAgent()
	files := make([]string, 60)
	for i := range files {
		files[i] = "file" + string(rune('a'+i%26)) + ".go"
	}
	msgs := a.buildMessages("main.go", "package main", "add tests", files)

	user := msgs[1].Content
	if !strings.Contains(user, "10 more files") {
		t.Error("user message should indicate omitted files when over cap")
	}
}

func TestPromptLoaderProjectOverride(t *testing.T) {
	dir := t.TempDir()
	// Create .project/prompts/system.md override
	promptDir := dir + "/.project/prompts"
	if err := mkdirAll(promptDir); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(promptDir+"/system.md", "You are a custom agent."); err != nil {
		t.Fatal(err)
	}

	loader := NewPromptLoader(dir)
	system := loader.SystemPrompt()
	if system != "You are a custom agent." {
		t.Errorf("expected project override, got: %s", system)
	}
}

func TestPromptLoaderFallsBackToEmbedded(t *testing.T) {
	loader := NewPromptLoader(t.TempDir()) // empty project dir
	system := loader.SystemPrompt()
	if !strings.Contains(system, "pair-programming agent") {
		t.Error("expected embedded default system prompt")
	}
}

func mkdirAll(path string) error {
	return os.MkdirAll(path, 0o755)
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
