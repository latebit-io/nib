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

// TestSystemPromptAutonomousModeRules verifies the autonomous-mode
// section ships the "stop asking permission" instructions. The
// Pac-Man rerun showed the LLM ending turns with "Say keep going
// and I'll continue Phase 3/4" — even at LevelTrusted/LevelYolo —
// because the prompt didn't explicitly forbid the offer-pattern.
// These keywords lock the fix in.
func TestSystemPromptAutonomousModeRules(t *testing.T) {
	t.Parallel()

	loader := NewPromptLoader("")

	autonomous := loader.SystemPrompt(SystemPromptData{Autonomous: true})
	for _, want := range []string{
		"Autonomous Mode",
		"WILL NOT be answered",
		"immediately activate the next pending task",
	} {
		if !strings.Contains(autonomous, want) {
			t.Errorf("autonomous prompt missing %q", want)
		}
	}

	// Ensure the ask-permission language is gated OUT in autonomous mode.
	if strings.Contains(autonomous, "ask the developer:") {
		t.Errorf("autonomous prompt still contains ask-permission instruction")
	}

	// Non-autonomous mode keeps the original ask-language. The
	// lint-violations ask only renders when CodingStyle is set
	// (it's inside the `## Style Lint` block) — so the test must
	// supply a non-nil CodingStyle to exercise that gate.
	guided := loader.SystemPrompt(SystemPromptData{
		Autonomous:  false,
		CodingStyle: &CodingStyleData{Name: "test", Rules: []string{}},
	})
	if strings.Contains(guided, "Autonomous Mode") {
		t.Errorf("non-autonomous prompt should not include the Autonomous Mode section")
	}
	if !strings.Contains(guided, "ask the developer:") {
		t.Errorf("non-autonomous prompt should retain the lint-violations ask-language")
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
	// 2026-04-27 prompt rev: rule was broadened from "ONE edit_file call
	// per step" to cover all file-edit tools (edit_file, write_file,
	// replace_file). The canonical phrase the test guards is now the
	// "One file-edit tool call per interactive turn" wording.
	if !strings.Contains(system, "One file-edit tool call per interactive turn") {
		t.Error("interactive system prompt should include the one-file-edit-per-turn rule")
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
	// Headless mode is autonomous; the one-file-edit-per-turn rule
	// (broadened 2026-04-27) is gated on `(not .Headless) (not .Autonomous)`,
	// so headless prompts must NOT include it.
	if strings.Contains(system, "One file-edit tool call per interactive turn") {
		t.Error("headless system prompt should not include single-edit-per-turn rule")
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
	// "Edit Strategy" was deleted in the 2026-04-26 prompt prune — its
	// content (minimal-surgical-edits guidance) was redundant on Claude
	// 4.x and now lives as a single rule in the trimmed Rules section.
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
	a.codingStyle = NewCodingStyleData("SOLID + Hexagonal", []StyleRule{
		{Name: "Single Responsibility", Instruction: "Each type has one reason to change.", Enforcement: "hard"},
		{Name: "Dependency Inversion", Instruction: "Depend on interfaces, not concretions.", Enforcement: "soft"},
	})
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
	if !strings.Contains(system, "[REQUIRED]") {
		t.Error("system prompt should render hard enforcement as [REQUIRED]")
	}
	if !strings.Contains(system, "[advisory]") {
		t.Error("system prompt should render soft enforcement as [advisory]")
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

// TestBuildMessagesUserPromptIncludesLanguage verifies the user message
// surfaces the file's language so the generating model applies style
// rules using native idioms. Lua files were silently picking up Go
// idioms because the prompt never told the model what language they
// were in.
func TestBuildMessagesUserPromptIncludesLanguage(t *testing.T) {
	a := testAgent()
	msgs := a.buildMessages("game.lua", "local M = {}", "add tests", nil, "", ModeExecution)

	user := msgs[1].Content
	if !strings.Contains(user, "language: Lua") {
		t.Errorf("user message should include language hint, got:\n%s", user)
	}
}

// TestBuildMessagesUserPromptOmitsLanguageWhenUnknown verifies we don't
// invent a language when the extension is unrecognized — a wrong guess
// is worse than no guess.
func TestBuildMessagesUserPromptOmitsLanguageWhenUnknown(t *testing.T) {
	a := testAgent()
	msgs := a.buildMessages("Makefile", "all:\n\techo hi", "add tests", nil, "", ModeExecution)

	user := msgs[1].Content
	if strings.Contains(user, "language:") {
		t.Errorf("user message should omit language hint for unknown extension, got:\n%s", user)
	}
}

// TestBuildMessagesCodingStyleHasLanguageGuidance verifies the system
// prompt teaches the model to apply rules using native-language idioms,
// not idioms borrowed from whichever language the rule wording most
// resembles. Without this, Clean Code rules read Go-flavored and the
// model produced Go-style scaffolding in Lua/Python files.
func TestBuildMessagesCodingStyleHasLanguageGuidance(t *testing.T) {
	a := testAgent()
	a.codingStyle = NewCodingStyleData("Clean Code", []StyleRule{
		{Name: "Errors as First-Class Citizens", Instruction: "Handle errors explicitly.", Enforcement: "hard"},
	})
	msgs := a.buildMessages("main.go", "package main", "add feature", nil, "", ModeExecution)

	system := msgs[0].Content
	if !strings.Contains(system, "Apply rules using idioms native to the file's language") {
		t.Error("system prompt should include native-language guidance when coding style is set")
	}
	if !strings.Contains(system, "do not write Go-style") {
		t.Error("system prompt should explicitly warn against Go-style returns in non-Go files")
	}
}

func TestNewCodingStyleData(t *testing.T) {
	rules := []StyleRule{
		{Name: "SRP", Instruction: "One reason to change", Enforcement: "hard"},
		{Name: "DIP", Instruction: "Depend on abstractions", Enforcement: "soft"},
	}
	data := NewCodingStyleData("Test Style", rules)

	if data.Name != "Test Style" {
		t.Errorf("Name = %q, want %q", data.Name, "Test Style")
	}
	if len(data.Rules) != 2 {
		t.Fatalf("len(Rules) = %d, want 2", len(data.Rules))
	}
	wantHard := "**SRP** [REQUIRED]: One reason to change"
	if data.Rules[0] != wantHard {
		t.Errorf("Rules[0] = %q, want %q", data.Rules[0], wantHard)
	}
	wantSoft := "**DIP** [advisory]: Depend on abstractions"
	if data.Rules[1] != wantSoft {
		t.Errorf("Rules[1] = %q, want %q", data.Rules[1], wantSoft)
	}
}

func TestBuildMessagesCodingStyleInPlanningMode(t *testing.T) {
	a := testAgent()
	a.codingStyle = NewCodingStyleData("DDD", []StyleRule{
		{Name: "Aggregates", Instruction: "Enforce invariants through roots.", Enforcement: "hard"},
	})
	msgs := a.buildMessages("main.go", "package main", "plan feature", nil, "", ModePlanning)

	system := msgs[0].Content
	if !strings.Contains(system, "Coding Style: DDD") {
		t.Error("planning prompt should include Coding Style section — style guides design too")
	}
	if !strings.Contains(system, "Aggregates") {
		t.Error("planning prompt should contain style rules")
	}
}

func TestBuildMessagesTerseInSystemPrompt(t *testing.T) {
	a := testAgent()
	a.SetTerse(true)
	msgs := a.buildMessages("main.go", "package main", "fix bug", nil, "", ModeExecution)

	system := msgs[0].Content
	if !strings.Contains(system, "Output Style — Terse") {
		t.Error("system prompt should include terse section when terse is enabled")
	}
	if !strings.Contains(system, "Minimize text") {
		t.Error("system prompt should contain terse instructions")
	}
}

func TestBuildMessagesNoTerse(t *testing.T) {
	a := testAgent()
	msgs := a.buildMessages("main.go", "package main", "fix bug", nil, "", ModeExecution)

	system := msgs[0].Content
	if strings.Contains(system, "Output Style — Terse") {
		t.Error("system prompt should not include terse section when terse is disabled")
	}
}

func TestBuildMessagesTerseInPlanningMode(t *testing.T) {
	a := testAgent()
	a.SetTerse(true)
	msgs := a.buildMessages("main.go", "package main", "plan feature", nil, "", ModePlanning)

	system := msgs[0].Content
	if !strings.Contains(system, "Output Style — Terse") {
		t.Error("planning prompt should include terse section when terse is enabled")
	}
	if !strings.Contains(system, "Plans and task lists should still be complete") {
		t.Error("planning terse should note that plans remain complete")
	}
}

func mkdirAll(path string) error {
	return os.MkdirAll(path, 0o755)
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
