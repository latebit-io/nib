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

func TestPromptLoaderBrokenOverrideFallsBackToEmbedded(t *testing.T) {
	dir := t.TempDir()
	promptDir := dir + "/.project/prompts"
	if err := mkdirAll(promptDir); err != nil {
		t.Fatal(err)
	}
	// Write a broken template that will fail to parse.
	if err := writeFile(promptDir+"/system.md.tmpl", "broken {{if}}"); err != nil {
		t.Fatal(err)
	}

	loader := NewPromptLoader(dir)
	system := loader.SystemPrompt(SystemPromptData{})
	if !strings.Contains(system, "pair-programming agent") {
		t.Error("broken project override should fall back to embedded default, got: " + system[:min(80, len(system))])
	}
	if strings.Contains(system, "broken") {
		t.Error("broken template syntax should not appear in output")
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
	if !strings.Contains(planning, ":skip") {
		t.Error("interactive planning prompt should mention :skip command")
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
	if strings.Contains(planning, ":skip") {
		t.Error("headless planning prompt should not mention :skip command")
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

func TestDetectDistributedMemory(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"team-demarkus", true},
		{"shared-docs", true},
		{"distributed-wiki", true},
		{"demarkus-soul", true},
		{"Team-Server", true},  // case-insensitive
		{"my-SHARED-db", true}, // keyword anywhere
		{"project-tools", false},
		{"memory", false},
		{"lsp-server", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DetectDistributedMemory([]string{tt.name})
			got := len(result) > 0
			if got != tt.want {
				t.Errorf("DetectDistributedMemory([%q]) returned %v, want match=%v", tt.name, result, tt.want)
			}
		})
	}
}

func TestDetectDistributedMemoryFiltersCorrectly(t *testing.T) {
	input := []string{"team-wiki", "lsp-server", "shared-docs", "my-linter"}
	got := DetectDistributedMemory(input)
	if len(got) != 2 {
		t.Fatalf("expected 2 distributed servers, got %d: %v", len(got), got)
	}
}

func TestSystemPromptDistributedMemoryPresent(t *testing.T) {
	loader := NewPromptLoader("")
	system := loader.SystemPrompt(SystemPromptData{
		DistributedMemory: []string{"team-demarkus"},
	})

	if !strings.Contains(system, "## Distributed Memory") {
		t.Error("system prompt should include Distributed Memory section when servers are configured")
	}
	if !strings.Contains(system, "team-demarkus") {
		t.Error("system prompt should list the distributed memory server name")
	}
	if !strings.Contains(system, "Publish only with developer approval") {
		t.Error("system prompt should include publish-with-approval guideline")
	}
}

func TestSystemPromptDistributedMemoryAbsent(t *testing.T) {
	loader := NewPromptLoader("")
	system := loader.SystemPrompt(SystemPromptData{})

	if strings.Contains(system, "Distributed Memory") {
		t.Error("system prompt should not include Distributed Memory section when no servers configured")
	}
}

func TestPlanningPromptDistributedMemoryPresent(t *testing.T) {
	loader := NewPromptLoader("")
	planning := loader.PlanningSystemPrompt(SystemPromptData{
		DistributedMemory: []string{"shared-wiki", "team-docs"},
	})

	if !strings.Contains(planning, "Distributed Memory") {
		t.Error("planning prompt should include Distributed Memory section when servers are configured")
	}
	if !strings.Contains(planning, "shared-wiki") {
		t.Error("planning prompt should list shared-wiki server")
	}
	if !strings.Contains(planning, "team-docs") {
		t.Error("planning prompt should list team-docs server")
	}
}

func TestPlanningPromptDistributedMemoryAbsent(t *testing.T) {
	loader := NewPromptLoader("")
	planning := loader.PlanningSystemPrompt(SystemPromptData{})

	if strings.Contains(planning, "Distributed Memory") {
		t.Error("planning prompt should not include Distributed Memory section when no servers configured")
	}
}

func TestBuildMessagesDistributedMemoryInSystemPrompt(t *testing.T) {
	a := testAgent()
	a.distributedMemory = []string{"team-server"}
	msgs := a.buildMessages("main.go", "package main", "add feature", nil, "", ModeExecution)

	system := msgs[0].Content
	if !strings.Contains(system, "Distributed Memory") {
		t.Error("system prompt should include Distributed Memory when agent has distributed servers")
	}
	if !strings.Contains(system, "team-server") {
		t.Error("system prompt should contain the distributed server name")
	}
}

func TestBuildMessagesNoDistributedMemory(t *testing.T) {
	a := testAgent()
	msgs := a.buildMessages("main.go", "package main", "add feature", nil, "", ModeExecution)

	system := msgs[0].Content
	if strings.Contains(system, "Distributed Memory") {
		t.Error("system prompt should not include Distributed Memory when no distributed servers")
	}
}

func TestBuildMessagesCodingStyleInSystemPrompt(t *testing.T) {
	a := testAgent()
	a.codingStyle = &CodingStyleData{
		Name: "SOLID + Hexagonal",
		Rules: []string{
			"**Single Responsibility**: Each type has one reason to change.",
			"**Dependency Inversion**: Depend on interfaces, not concretions.",
		},
	}
	msgs := a.buildMessages("main.go", "package main", "add feature", nil, "", ModeExecution)

	system := msgs[0].Content
	if !strings.Contains(system, "Coding Style: SOLID + Hexagonal") {
		t.Error("system prompt should include Coding Style section when style is configured")
	}
	if !strings.Contains(system, "Single Responsibility") {
		t.Error("system prompt should contain style rule names")
	}
	if !strings.Contains(system, "Dependency Inversion") {
		t.Error("system prompt should contain all style rules")
	}
	if !strings.Contains(system, "runtime constraints") {
		t.Error("system prompt should explain that rules are constraints")
	}
}

func TestBuildMessagesNoCodingStyle(t *testing.T) {
	a := testAgent()
	msgs := a.buildMessages("main.go", "package main", "add feature", nil, "", ModeExecution)

	system := msgs[0].Content
	if strings.Contains(system, "Coding Style") {
		t.Error("system prompt should not include Coding Style section when no style configured")
	}
}

func TestNewCodingStyleData(t *testing.T) {
	rules := []StyleRule{
		{Name: "SRP", Instruction: "One reason to change"},
		{Name: "DIP", Instruction: "Depend on abstractions"},
	}
	data := NewCodingStyleData("Test Style", rules)

	if data.Name != "Test Style" {
		t.Errorf("Name = %q, want %q", data.Name, "Test Style")
	}
	if len(data.Rules) != 2 {
		t.Fatalf("len(Rules) = %d, want 2", len(data.Rules))
	}
	if data.Rules[0] != "**SRP**: One reason to change" {
		t.Errorf("Rules[0] = %q, want %q", data.Rules[0], "**SRP**: One reason to change")
	}
	if data.Rules[1] != "**DIP**: Depend on abstractions" {
		t.Errorf("Rules[1] = %q, want %q", data.Rules[1], "**DIP**: Depend on abstractions")
	}
}

func TestBuildMessagesCodingStyleInPlanningMode(t *testing.T) {
	a := testAgent()
	a.codingStyle = &CodingStyleData{
		Name:  "DDD",
		Rules: []string{"**Aggregates**: Enforce invariants through roots."},
	}
	msgs := a.buildMessages("main.go", "package main", "plan feature", nil, "", ModePlanning)

	system := msgs[0].Content
	if !strings.Contains(system, "Coding Style: DDD") {
		t.Error("planning prompt should include Coding Style section — style guides design too")
	}
	if !strings.Contains(system, "Aggregates") {
		t.Error("planning prompt should contain style rules")
	}
}

func mkdirAll(path string) error {
	return os.MkdirAll(path, 0o755)
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
