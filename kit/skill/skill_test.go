package skill

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/contracttest"
)

// writeSkill creates <root>/<dir>/SKILL.md with the given content.
func writeSkill(t *testing.T, root, dir, content string) {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", d, err)
	}
	if err := os.WriteFile(filepath.Join(d, skillFile), []byte(content), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
}

func TestLoad_ParsesFrontmatterAndBody(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "code-review", `---
name: code-review
description: Review a Go diff for correctness and layering.
---
Check for layering violations and missing edge cases.
`)
	skills, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("got %d skills, want 1", len(skills))
	}
	s := skills[0]
	if s.Name != "code-review" {
		t.Errorf("Name = %q, want code-review", s.Name)
	}
	if s.Description != "Review a Go diff for correctness and layering." {
		t.Errorf("Description = %q", s.Description)
	}
	if s.Body != "Check for layering violations and missing edge cases." {
		t.Errorf("Body = %q", s.Body)
	}
}

func TestLoad_NameFallsBackToDir(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "commit-style", `---
description: Encode the repo's commit conventions.
---
Use conventional commits.
`)
	skills, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(skills) != 1 || skills[0].Name != "commit-style" {
		t.Fatalf("expected name fallback to dir basename, got %+v", skills)
	}
}

func TestLoad_MissingRootIsNotAnError(t *testing.T) {
	skills, err := Load(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing root should not error, got %v", err)
	}
	if skills != nil {
		t.Fatalf("missing root should yield nil skills, got %v", skills)
	}
}

func TestLoad_RejectsMissingDescription(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "nodesc", `---
name: nodesc
---
body only
`)
	skills, err := Load(root)
	if err == nil {
		t.Fatal("expected error for missing description")
	}
	if len(skills) != 0 {
		t.Fatalf("malformed skill should not load, got %v", skills)
	}
}

func TestLoad_RejectsInvalidName(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "bad name!", `---
description: has spaces and bang in the dir-derived name.
---
body
`)
	_, err := Load(root)
	if err == nil {
		t.Fatal("expected error for invalid name")
	}
}

func TestLoad_SubdirWithoutSkillFileSkipped(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "real", `---
description: a real skill.
---
do the thing
`)
	// A sibling dir with no SKILL.md (e.g. shared resources) is ignored.
	if err := os.MkdirAll(filepath.Join(root, "resources"), 0o755); err != nil {
		t.Fatal(err)
	}
	skills, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("got %d skills, want 1 (resources dir skipped)", len(skills))
	}
}

func TestLoad_PartialFailureReturnsGoodSkillsAndError(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "good", `---
description: a good one.
---
ok
`)
	writeSkill(t, root, "bad", `---
name: bad
---
no description
`)
	skills, err := Load(root)
	if err == nil {
		t.Fatal("expected joined error for the bad skill")
	}
	if len(skills) != 1 || skills[0].Name != "good" {
		t.Fatalf("expected the good skill to still load, got %+v", skills)
	}
}

func TestNeedsShell(t *testing.T) {
	cases := []struct {
		tools []string
		want  bool
	}{
		{nil, false},
		{[]string{"Read", "Grep"}, false},
		{[]string{"Bash"}, true},
		{[]string{"Read", "shell"}, true},
		{[]string{"execute_command"}, true},
	}
	for _, c := range cases {
		got := Skill{AllowedTools: c.tools}.NeedsShell()
		if got != c.want {
			t.Errorf("NeedsShell(%v) = %v, want %v", c.tools, got, c.want)
		}
	}
}

func TestDiscover_AdaptsPromptSkillsRefusesScriptSkills(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "prompt-only", `---
description: pure prompt skill.
---
instructions here
`)
	writeSkill(t, root, "scripted", `---
description: needs shell.
allowed-tools: [Bash]
---
runs a script
`)
	res, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(res.Tools) != 1 || len(res.Loaded) != 1 || res.Loaded[0] != "prompt-only" {
		t.Fatalf("expected only prompt-only loaded, got Loaded=%v", res.Loaded)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "scripted" {
		t.Fatalf("expected scripted skipped, got Skipped=%v", res.Skipped)
	}
}

func TestAdaptTool_NamePrefixAndBody(t *testing.T) {
	tool := adaptTool(Skill{Name: "code-review", Description: "d", Body: "the body"})
	def := tool.Definition()
	if def.Function.Name != "skill_code-review" {
		t.Errorf("tool name = %q, want skill_code-review", def.Function.Name)
	}
	res := tool.Execute(context.Background(), llm.ToolCall{})
	if res.Content != "the body" {
		t.Errorf("Execute content = %q, want the body", res.Content)
	}
	if res.IsError {
		t.Error("skill tool should never report IsError")
	}
}

// TestSkillToolContract runs the kit.Tool contract suite against the
// skill adapter.
func TestSkillToolContract(t *testing.T) {
	contracttest.Tool(t, func() kit.Tool {
		return adaptTool(Skill{
			Name:        "contract",
			Description: "contract test skill",
			Body:        "body",
		})
	})
}
