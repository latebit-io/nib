package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	skills, err := Load(root, SourceProject)
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
	skills, err := Load(root, SourceProject)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(skills) != 1 || skills[0].Name != "commit-style" {
		t.Fatalf("expected name fallback to dir basename, got %+v", skills)
	}
}

func TestLoad_MissingRootIsNotAnError(t *testing.T) {
	skills, err := Load(filepath.Join(t.TempDir(), "does-not-exist"), SourceProject)
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
	skills, err := Load(root, SourceProject)
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
	_, err := Load(root, SourceProject)
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
	skills, err := Load(root, SourceProject)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("got %d skills, want 1 (resources dir skipped)", len(skills))
	}
}

func TestLoad_RejectsOversizeFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "huge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A header that would parse fine, followed by a body over the cap.
	body := strings.Repeat("x", maxSkillFileBytes+1)
	content := "---\ndescription: huge.\n---\n" + body
	if err := os.WriteFile(filepath.Join(dir, skillFile), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	skills, err := Load(root, SourceProject)
	if err == nil {
		t.Fatal("expected an error for an oversize SKILL.md")
	}
	if len(skills) != 0 {
		t.Fatalf("oversize skill should not load, got %v", skills)
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
	skills, err := Load(root, SourceProject)
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
		// False-positive guards: substring matching on "sh"/"run" would
		// have wrongly flagged these.
		{[]string{"publish"}, false},
		{[]string{"push"}, false},
		{[]string{"refresh"}, false},
		{[]string{"run_tests"}, false},
		{[]string{"rerun"}, false},
		{[]string{"memory_publish", "find_references"}, false},
	}
	for _, c := range cases {
		got := Skill{AllowedTools: c.tools}.NeedsShell()
		if got != c.want {
			t.Errorf("NeedsShell(%v) = %v, want %v", c.tools, got, c.want)
		}
	}
}

// newProject creates a temp project root and points the global skills
// dir at a sibling temp dir (so Discover never reads the developer's
// real ~/.config/nib/skills). Returns (projectRoot, globalDir); project
// skills go under ProjectDir(projectRoot), global under globalDir.
func newProject(t *testing.T) (projectRoot, globalDir string) {
	t.Helper()
	projectRoot = t.TempDir()
	globalDir = t.TempDir()
	t.Setenv("NIB_GLOBAL_SKILLS_DIR", globalDir)
	return projectRoot, globalDir
}

func TestDiscover_AdaptsPromptSkillsRefusesScriptSkills(t *testing.T) {
	projectRoot, _ := newProject(t)
	skillsDir := ProjectDir(projectRoot)
	writeSkill(t, skillsDir, "prompt-only", `---
description: pure prompt skill.
---
instructions here
`)
	writeSkill(t, skillsDir, "scripted", `---
description: needs shell.
allowed-tools: [Bash]
---
runs a script
`)
	res, err := Discover(projectRoot)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(res.Tools) != 1 || len(res.Loaded) != 1 || res.Loaded[0].Name != "prompt-only" {
		t.Fatalf("expected only prompt-only loaded, got Loaded=%v", res.Loaded)
	}
	if res.Loaded[0].Source != SourceProject {
		t.Errorf("loaded skill source = %q, want project", res.Loaded[0].Source)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Name != "scripted" {
		t.Fatalf("expected scripted skipped, got Skipped=%v", res.Skipped)
	}
}

func TestDiscover_LoadsGlobalAndProject(t *testing.T) {
	projectRoot, globalDir := newProject(t)
	writeSkill(t, ProjectDir(projectRoot), "proj-skill", "---\ndescription: project one.\n---\nP")
	writeSkill(t, globalDir, "glob-skill", "---\ndescription: global one.\n---\nG")

	res, err := Discover(projectRoot)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	got := map[string]Source{}
	for _, s := range res.Loaded {
		got[s.Name] = s.Source
	}
	if got["proj-skill"] != SourceProject || got["glob-skill"] != SourceGlobal {
		t.Fatalf("expected both layers loaded with correct source, got %v", got)
	}
}

func TestDiscover_ProjectShadowsGlobal(t *testing.T) {
	projectRoot, globalDir := newProject(t)
	writeSkill(t, ProjectDir(projectRoot), "code-review", "---\ndescription: PROJECT version.\n---\nproject body")
	writeSkill(t, globalDir, "code-review", "---\ndescription: GLOBAL version.\n---\nglobal body")

	res, err := Discover(projectRoot)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(res.Loaded) != 1 || res.Loaded[0].Source != SourceProject {
		t.Fatalf("expected the project skill to win, got %+v", res.Loaded)
	}
	if res.Loaded[0].Description != "PROJECT version." {
		t.Errorf("winner description = %q, want PROJECT version.", res.Loaded[0].Description)
	}
	if len(res.Shadowed) != 1 || res.Shadowed[0].Source != SourceGlobal {
		t.Fatalf("expected the global skill shadowed, got %+v", res.Shadowed)
	}
}

func TestDiscover_NoSkillsAnywhereIsClean(t *testing.T) {
	projectRoot, _ := newProject(t) // global points at an empty temp dir
	res, err := Discover(projectRoot)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(res.Tools) != 0 || len(res.Loaded) != 0 || len(res.Skipped) != 0 || len(res.Shadowed) != 0 {
		t.Fatalf("expected empty result with no skills, got %+v", res)
	}
}

func TestMerge_FirstLayerWins(t *testing.T) {
	project := []Skill{{Name: "a", Source: SourceProject}, {Name: "b", Source: SourceProject}}
	global := []Skill{{Name: "b", Source: SourceGlobal}, {Name: "c", Source: SourceGlobal}}

	winners, shadowed := Merge(project, global)

	gotWin := map[string]Source{}
	for _, s := range winners {
		gotWin[s.Name] = s.Source
	}
	if len(winners) != 3 || gotWin["a"] != SourceProject || gotWin["b"] != SourceProject || gotWin["c"] != SourceGlobal {
		t.Fatalf("merge winners wrong: %v", gotWin)
	}
	if len(shadowed) != 1 || shadowed[0].Name != "b" || shadowed[0].Source != SourceGlobal {
		t.Fatalf("expected global b shadowed, got %+v", shadowed)
	}
}

func TestProjectDir(t *testing.T) {
	got := ProjectDir(filepath.Join("tmp", "proj"))
	want := filepath.Join("tmp", "proj", ".project", "skills")
	if got != want {
		t.Errorf("ProjectDir = %q, want %q", got, want)
	}
}

func TestGlobalDir_EnvOverride(t *testing.T) {
	t.Setenv("NIB_GLOBAL_SKILLS_DIR", filepath.Join("custom", "skills"))
	dir, ok := GlobalDir()
	if !ok || dir != filepath.Join("custom", "skills") {
		t.Fatalf("GlobalDir with override = (%q, %v), want (custom/skills, true)", dir, ok)
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
