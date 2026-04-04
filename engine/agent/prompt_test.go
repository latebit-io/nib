package agent

import (
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// promptTestWorkspace is a minimal Workspace for prompt tests.
type promptTestWorkspace struct{}

func (promptTestWorkspace) ProjectRoot() string               { return "" }
func (promptTestWorkspace) ReadFile(_ string) (string, error) { return "", nil }
func (promptTestWorkspace) ListFiles() ([]string, error)      { return nil, nil }
func (promptTestWorkspace) WriteFile(_, _ string) error       { return nil }
func (promptTestWorkspace) CanonPath(p string) string         { return p }
func (promptTestWorkspace) InContext(_ string) bool           { return true }
func (promptTestWorkspace) AddContext(_ string)               {}

// testAgent returns a minimal agent with embedded prompts (no project overrides).
func testAgent() *Agent {
	return &Agent{
		prompts:   NewPromptLoader(""),
		workspace: promptTestWorkspace{},
		tools:     make(map[string]Tool),
	}
}

func TestBuildMessagesIncludesContextSet(t *testing.T) {
	a := testAgent()
	contextFiles := []string{"src/auth.go", "src/handler.go"}
	msgs := a.buildMessages("main.go", "package main", "add tests", contextFiles, "", ModeExecution)

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

func TestBuildMessagesIncludesMemorySummary(t *testing.T) {
	a := testAgent()
	msgs := a.buildMessages("main.go", "package main", "add tests", nil, "Key decision: use hexagonal arch.", ModeExecution)

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	user := msgs[1].Content
	if !strings.Contains(user, "Memory Summary") {
		t.Error("user message should contain Memory Summary section")
	}
	if !strings.Contains(user, "Key decision: use hexagonal arch.") {
		t.Error("user message should contain the summary text")
	}
}

func TestBuildMessagesMemorySummaryTruncated(t *testing.T) {
	t.Run("ascii", func(t *testing.T) {
		a := testAgent()
		big := strings.Repeat("x", maxMemorySummaryBytes+100)
		msgs := a.buildMessages("main.go", "package main", "add tests", nil, big, ModeExecution)

		user := msgs[1].Content
		if !strings.Contains(user, "[truncated]") {
			t.Error("oversized summary should be truncated with [truncated] marker")
		}
		if strings.Contains(user, strings.Repeat("x", maxMemorySummaryBytes+1)) {
			t.Error("full oversized summary should not appear in prompt")
		}
	})

	t.Run("multibyte rune boundary", func(t *testing.T) {
		a := testAgent()
		// U+4E16 (世) is 3 bytes in UTF-8. Fill past the limit so the cut
		// point is likely mid-rune if not handled correctly.
		big := strings.Repeat("世", maxMemorySummaryBytes)
		msgs := a.buildMessages("main.go", "package main", "add tests", nil, big, ModeExecution)

		user := msgs[1].Content
		if !strings.Contains(user, "[truncated]") {
			t.Error("oversized multibyte summary should be truncated")
		}
		if !utf8.ValidString(user) {
			t.Error("truncated prompt contains invalid UTF-8")
		}
	})
}

func TestBuildMessagesNoMemorySummary(t *testing.T) {
	a := testAgent() // promptTestWorkspace returns ""
	msgs := a.buildMessages("main.go", "package main", "add tests", nil, "", ModeExecution)

	user := msgs[1].Content
	if strings.Contains(user, "Memory Summary") {
		t.Error("user message should not contain Memory Summary section when summary is empty")
	}
}

func TestBuildMessagesNoContextSet(t *testing.T) {
	a := testAgent()
	msgs := a.buildMessages("main.go", "package main", "add tests", nil, "", ModeExecution)

	user := msgs[1].Content
	if strings.Contains(user, "Context Set") {
		t.Error("user message should not contain context set section when no context files")
	}
}

func TestBuildMessagesSystemPromptIncludesContextSetRules(t *testing.T) {
	a := testAgent()
	msgs := a.buildMessages("main.go", "package main", "add tests", nil, "", ModeExecution)

	system := msgs[0].Content
	if !strings.Contains(system, "Context Set") {
		t.Error("system prompt should include context set section")
	}
}

func TestBuildMessagesSystemPromptIncludesCriticalPerspective(t *testing.T) {
	a := testAgent()
	msgs := a.buildMessages("main.go", "package main", "add tests", nil, "", ModeExecution)

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
	msgs := a.buildMessages("main.go", "package main", "add tests", files, "", ModeExecution)

	user := msgs[1].Content
	if !strings.Contains(user, "10 more files") {
		t.Error("user message should indicate omitted files when over cap")
	}
}

func TestPromptLoaderProjectOverride(t *testing.T) {
	dir := t.TempDir()
	// Create .project/prompts/system.md.tmpl override
	promptDir := dir + "/.project/prompts"
	if err := mkdirAll(promptDir); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(promptDir+"/system.md.tmpl", "You are a custom agent."); err != nil {
		t.Fatal(err)
	}

	loader := NewPromptLoader(dir)
	system := loader.SystemPrompt(SystemPromptData{})
	if system != "You are a custom agent." {
		t.Errorf("expected project override, got: %s", system)
	}
}

func TestPromptLoaderFallsBackToEmbedded(t *testing.T) {
	loader := NewPromptLoader(t.TempDir()) // empty project dir
	system := loader.SystemPrompt(SystemPromptData{})
	if !strings.Contains(system, "pair-programming agent") {
		t.Error("expected embedded default system prompt")
	}
}

func TestSystemPromptIncludesMemorySection(t *testing.T) {
	loader := NewPromptLoader("")
	system := loader.SystemPrompt(SystemPromptData{})
	if !strings.Contains(system, "## Memory") {
		t.Error("system prompt should include Memory section")
	}
}

func TestSystemPromptInteractiveMode(t *testing.T) {
	loader := NewPromptLoader("")
	system := loader.SystemPrompt(SystemPromptData{Headless: false})

	if !strings.Contains(system, "pair-programming agent") {
		t.Error("interactive system prompt should contain 'pair-programming agent'")
	}
	if !strings.Contains(system, "Context Set") {
		t.Error("interactive system prompt should include Context Set section")
	}
	if !strings.Contains(system, "ONE edit_file call per step") {
		t.Error("interactive system prompt should include single-edit rule")
	}
	if strings.Contains(system, "headless mode") {
		t.Error("interactive system prompt should not mention headless mode")
	}
}

func TestSystemPromptHeadlessMode(t *testing.T) {
	loader := NewPromptLoader("")
	system := loader.SystemPrompt(SystemPromptData{Headless: true})

	if !strings.Contains(system, "autonomous coding agent") {
		t.Error("headless system prompt should contain 'autonomous coding agent'")
	}
	if !strings.Contains(system, "headless mode") {
		t.Error("headless system prompt should mention headless mode")
	}
	if strings.Contains(system, "pair-programming agent") {
		t.Error("headless system prompt should not contain 'pair-programming agent'")
	}
	if strings.Contains(system, "Context Set") {
		t.Error("headless system prompt should not include Context Set section")
	}
	if strings.Contains(system, "ONE edit_file call per step") {
		t.Error("headless system prompt should not include single-edit rule")
	}
	if strings.Contains(system, "After a rejection") {
		t.Error("headless system prompt should not include rejection rules")
	}
	// Shared sections should still be present.
	if !strings.Contains(system, "## Memory") {
		t.Error("headless system prompt should include Memory section")
	}
	if !strings.Contains(system, "Critical Perspective") {
		t.Error("headless system prompt should include Critical Perspective section")
	}
	if !strings.Contains(system, "Edit Strategy") {
		t.Error("headless system prompt should include Edit Strategy section")
	}
}

func TestPlanningPromptInteractiveMode(t *testing.T) {
	loader := NewPromptLoader("")
	planning := loader.PlanningSystemPrompt(SystemPromptData{Headless: false})

	if !strings.Contains(planning, "pair-programming agent") {
		t.Error("interactive planning prompt should contain 'pair-programming agent'")
	}
	if !strings.Contains(planning, ":done") {
		t.Error("interactive planning prompt should mention :done command")
	}
}

func TestPlanningPromptHeadlessMode(t *testing.T) {
	loader := NewPromptLoader("")
	planning := loader.PlanningSystemPrompt(SystemPromptData{Headless: true})

	if !strings.Contains(planning, "autonomous planning agent") {
		t.Error("headless planning prompt should contain 'autonomous planning agent'")
	}
	if strings.Contains(planning, "pair-programming agent") {
		t.Error("headless planning prompt should not contain 'pair-programming agent'")
	}
	if strings.Contains(planning, ":done") {
		t.Error("headless planning prompt should not mention :done command")
	}
	// Shared sections should still be present.
	if !strings.Contains(planning, "Plan Schema") {
		t.Error("headless planning prompt should include Plan Schema section")
	}
}

func TestBuildMessagesHeadlessMode(t *testing.T) {
	a := testAgent()
	a.interactionMode = Headless
	msgs := a.buildMessages("main.go", "package main", "fix bug", nil, "", ModeExecution)

	system := msgs[0].Content
	if !strings.Contains(system, "autonomous coding agent") {
		t.Error("headless buildMessages should produce headless system prompt")
	}
	if strings.Contains(system, "pair-programming agent") {
		t.Error("headless buildMessages should not produce interactive system prompt")
	}
}

func mkdirAll(path string) error {
	return os.MkdirAll(path, 0o755)
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
