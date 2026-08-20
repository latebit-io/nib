package agent

import (
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/coding/prompts"
	codingtools "github.com/latebit-io/nib/coding/tools"
)

// promptTestWorkspace is a minimal Workspace for prompt tests.
type promptTestWorkspace struct{}

func (promptTestWorkspace) ProjectRoot() string               { return "" }
func (promptTestWorkspace) ReadFile(_ string) (string, error) { return "", nil }
func (promptTestWorkspace) ListFiles() ([]string, error)      { return nil, nil }
func (promptTestWorkspace) WriteFile(_, _ string) error       { return nil }
func (promptTestWorkspace) CanonPath(p string) string         { return p }

// testAgent returns a minimal agent with embedded prompts (no project overrides).
func testAgent() *Agent {
	return &Agent{
		prompts:   prompts.NewPromptLoader(""),
		workspace: promptTestWorkspace{},
		tools:     make(map[string]Tool),
	}
}

func TestBuildMessagesIncludesMemorySummary(t *testing.T) {
	a := testAgent()
	msgs := a.buildMessages("main.go", "package main", "add tests", "Key decision: use hexagonal arch.", event.ModeExecution)

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
		msgs := a.buildMessages("main.go", "package main", "add tests", big, event.ModeExecution)

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
		msgs := a.buildMessages("main.go", "package main", "add tests", big, event.ModeExecution)

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
	msgs := a.buildMessages("main.go", "package main", "add tests", "", event.ModeExecution)

	user := msgs[1].Content
	if strings.Contains(user, "Memory Summary") {
		t.Error("user message should not contain Memory Summary section when summary is empty")
	}
}

func TestBuildMessagesSystemPromptIncludesCriticalPerspective(t *testing.T) {
	a := testAgent()
	msgs := a.buildMessages("main.go", "package main", "add tests", "", event.ModeExecution)

	system := msgs[0].Content
	if !strings.Contains(system, "Critical Perspective") {
		t.Error("system prompt should include critical perspective section")
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

	loader := prompts.NewPromptLoader(dir)
	system := loader.SystemPrompt(prompts.SystemPromptData{})
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

	loader := prompts.NewPromptLoader(dir)
	system := loader.SystemPrompt(prompts.SystemPromptData{})
	if !strings.Contains(system, "The developer steers") {
		t.Error("broken project override should fall back to embedded default, got: " + system[:min(80, len(system))])
	}
	if strings.Contains(system, "broken") {
		t.Error("broken template syntax should not appear in output")
	}
}

func TestPromptLoaderFallsBackToEmbedded(t *testing.T) {
	loader := prompts.NewPromptLoader(t.TempDir()) // empty project dir
	system := loader.SystemPrompt(prompts.SystemPromptData{})
	if !strings.Contains(system, "The developer steers") {
		t.Error("expected embedded default system prompt")
	}
}

// TestSystemPromptAutonomyRules verifies the Autonomy section ships
// the "stop asking permission" instructions. The Pac-Man rerun
// showed the LLM ending turns with "Say keep going and I'll continue
// Phase 3/4" because the prompt didn't explicitly forbid the offer-
// pattern. These keywords lock the fix in.
func TestSystemPromptAutonomyRules(t *testing.T) {
	t.Parallel()

	loader := prompts.NewPromptLoader("")
	system := loader.SystemPrompt(prompts.SystemPromptData{})
	for _, want := range []string{
		"## Autonomy",
		"WILL NOT be answered",
		// The tool now auto-activates the next task on complete, so the
		// prompt instruction shifted from "you must activate" to "the
		// tool already did it." Keep verifying the section tells the LLM
		// to keep moving at task boundaries — that's the load-bearing
		// intent for the autonomous-loop fix.
		"auto-activates the next pending task",
	} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}

// TestSystemPromptDistinguishesProjectInitFromMemoryPublish locks the
// cross-tool guidance that prevents the Pac-Man-eval failure where the
// LLM used memory_publish to bootstrap /project.md, then got stuck
// because the work tree gate did not reload. The disambiguation has
// moved twice: off project_init's tool description into the template's
// hardcoded Tool Notes (cross-tool guidance is prompt-level, not
// schema-level), then — with tool-owned prompt snippets — onto
// [tools.ProjectInitTool.PromptGuidelines], so it renders exactly when
// the tool is registered. This test locks the full path: the tool
// contributes the bullet, and the template renders it.
func TestSystemPromptDistinguishesProjectInitFromMemoryPublish(t *testing.T) {
	t.Parallel()

	notes := (&codingtools.ProjectInitTool{}).PromptGuidelines()
	loader := prompts.NewPromptLoader("")
	system := loader.SystemPrompt(prompts.SystemPromptData{ToolNotes: notes})
	for _, want := range []string{"project_init", "memory_publish", "/project.md"} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt missing %q (tool-disambiguation guidance)", want)
		}
	}
}

// TestToolNotesCollectedFromRegisteredTools locks the tool-owned
// prompt-snippet path at the agent level: a constructed agent's
// toolNotes() carries the bullets its registered built-ins contribute,
// in tool-definition order, without the template hardcoding any of
// them. Guards the BeforePark-style failure where a surface exists but
// the merge/collection layer silently drops it.
func TestToolNotesCollectedFromRegisteredTools(t *testing.T) {
	t.Parallel()

	ag := New(&multiTurnProvider{}, stubWorkspace{}, nil)
	t.Cleanup(ag.Close)
	notes := ag.toolNotes()
	if len(notes) == 0 {
		t.Fatal("toolNotes() returned nothing; collection layer is dropping tool guidance")
	}
	joined := strings.Join(notes, "\n")
	for _, want := range []string{"edit_file", "search_project", "glob"} {
		if !strings.Contains(joined, want) {
			t.Errorf("toolNotes() missing guidance mentioning %q", want)
		}
	}
	rendered := ag.rebuildSystemPrompt(event.ModeExecution)
	if !strings.Contains(rendered, "## Tool Notes") {
		t.Error("rendered system prompt missing Tool Notes section")
	}
	if !strings.Contains(rendered, notes[0]) {
		t.Error("rendered system prompt missing first collected tool note")
	}
}

func TestSystemPromptIncludesMemorySection(t *testing.T) {
	loader := prompts.NewPromptLoader("")
	system := loader.SystemPrompt(prompts.SystemPromptData{})
	if !strings.Contains(system, "## Memory") {
		t.Error("system prompt should include Memory section")
	}
}

func TestSystemPromptInteractiveMode(t *testing.T) {
	loader := prompts.NewPromptLoader("")
	system := loader.SystemPrompt(prompts.SystemPromptData{Headless: false})

	if !strings.Contains(system, "The developer steers") {
		t.Error("interactive system prompt should contain 'The developer steers'")
	}
	if strings.Contains(system, "headless mode") {
		t.Error("interactive system prompt should not mention headless mode")
	}
}

func TestSystemPromptHeadlessMode(t *testing.T) {
	loader := prompts.NewPromptLoader("")
	system := loader.SystemPrompt(prompts.SystemPromptData{Headless: true})

	if !strings.Contains(system, "autonomous coding agent") {
		t.Error("headless system prompt should contain 'autonomous coding agent'")
	}
	if !strings.Contains(system, "headless mode") {
		t.Error("headless system prompt should mention headless mode")
	}
	if strings.Contains(system, "The developer steers") {
		t.Error("headless system prompt should not contain 'The developer steers'")
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
	loader := prompts.NewPromptLoader("")
	planning := loader.PlanningSystemPrompt(prompts.SystemPromptData{Headless: false})

	if !strings.Contains(planning, "The developer steers") {
		t.Error("interactive planning prompt should contain 'The developer steers'")
	}
	if !strings.Contains(planning, ":done") {
		t.Error("interactive planning prompt should mention :done command")
	}
	if !strings.Contains(planning, ":skip") {
		t.Error("interactive planning prompt should mention :skip command")
	}
}

func TestPlanningPromptHeadlessMode(t *testing.T) {
	loader := prompts.NewPromptLoader("")
	planning := loader.PlanningSystemPrompt(prompts.SystemPromptData{Headless: true})

	if !strings.Contains(planning, "autonomous planning agent") {
		t.Error("headless planning prompt should contain 'autonomous planning agent'")
	}
	if strings.Contains(planning, "The developer steers") {
		t.Error("headless planning prompt should not contain 'The developer steers'")
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
	msgs := a.buildMessages("main.go", "package main", "fix bug", "", event.ModeExecution)

	system := msgs[0].Content
	if !strings.Contains(system, "autonomous coding agent") {
		t.Error("headless buildMessages should produce headless system prompt")
	}
	if strings.Contains(system, "The developer steers") {
		t.Error("headless buildMessages should not produce interactive system prompt")
	}
}

func TestSystemPromptDistributedMemoryPresent(t *testing.T) {
	loader := prompts.NewPromptLoader("")
	system := loader.SystemPrompt(prompts.SystemPromptData{
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
	loader := prompts.NewPromptLoader("")
	system := loader.SystemPrompt(prompts.SystemPromptData{})

	if strings.Contains(system, "Distributed Memory") {
		t.Error("system prompt should not include Distributed Memory section when no servers configured")
	}
}

func TestPlanningPromptDistributedMemoryPresent(t *testing.T) {
	loader := prompts.NewPromptLoader("")
	planning := loader.PlanningSystemPrompt(prompts.SystemPromptData{
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
	loader := prompts.NewPromptLoader("")
	planning := loader.PlanningSystemPrompt(prompts.SystemPromptData{})

	if strings.Contains(planning, "Distributed Memory") {
		t.Error("planning prompt should not include Distributed Memory section when no servers configured")
	}
}

func TestBuildMessagesDistributedMemoryInSystemPrompt(t *testing.T) {
	a := testAgent()
	a.distributedMemory = []string{"team-server"}
	msgs := a.buildMessages("main.go", "package main", "add feature", "", event.ModeExecution)

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
	msgs := a.buildMessages("main.go", "package main", "add feature", "", event.ModeExecution)

	system := msgs[0].Content
	if strings.Contains(system, "Distributed Memory") {
		t.Error("system prompt should not include Distributed Memory when no distributed servers")
	}
}

// TestBuildMessagesUserPromptIncludesLanguage verifies the user message
// surfaces the file's language so the generating model applies style
// rules using native idioms. Lua files were silently picking up Go
// idioms because the prompt never told the model what language they
// were in.
func TestBuildMessagesUserPromptIncludesLanguage(t *testing.T) {
	a := testAgent()
	msgs := a.buildMessages("game.lua", "local M = {}", "add tests", "", event.ModeExecution)

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
	msgs := a.buildMessages("Makefile", "all:\n\techo hi", "add tests", "", event.ModeExecution)

	user := msgs[1].Content
	if strings.Contains(user, "language:") {
		t.Errorf("user message should omit language hint for unknown extension, got:\n%s", user)
	}
}

func TestBuildMessagesTerseInSystemPrompt(t *testing.T) {
	a := testAgent()
	a.SetTerse(true)
	msgs := a.buildMessages("main.go", "package main", "fix bug", "", event.ModeExecution)

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
	msgs := a.buildMessages("main.go", "package main", "fix bug", "", event.ModeExecution)

	system := msgs[0].Content
	if strings.Contains(system, "Output Style — Terse") {
		t.Error("system prompt should not include terse section when terse is disabled")
	}
}

func TestBuildMessagesTerseInPlanningMode(t *testing.T) {
	a := testAgent()
	a.SetTerse(true)
	msgs := a.buildMessages("main.go", "package main", "plan feature", "", event.ModePlanning)

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

// TestContextFilesFlowIntoSystemPrompt locks the NewOptions.ContextFiles
// → rendered-prompt path: a repo-carried AGENTS.md injected at
// construction appears in every (re)built system prompt behind the
// trust boundary.
func TestContextFilesFlowIntoSystemPrompt(t *testing.T) {
	t.Parallel()

	ag := New(&multiTurnProvider{}, stubWorkspace{}, &NewOptions{
		ContextFiles: []prompts.ContextFile{{Path: "/repo/AGENTS.md", Content: "always run make lint"}},
	})
	t.Cleanup(ag.Close)
	system := ag.rebuildSystemPrompt(event.ModeExecution)
	for _, want := range []string{"## Project Instructions", "/repo/AGENTS.md", "always run make lint"} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}
