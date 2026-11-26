package skill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// isolateUser points both user-global skill directories at temp dirs,
// so Discover never reads the developer's real ones. Returns the nib
// global dir; the shared one is SharedGlobalDir().
func isolateUser(t *testing.T) (globalDir string) {
	t.Helper()
	globalDir = t.TempDir()
	t.Setenv("NIB_GLOBAL_SKILLS_DIR", globalDir)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	return globalDir
}

// newProject creates a temp project root with the user-global skill
// directories isolated. Returns (projectRoot, globalDir); project
// skills go under ProjectDir(projectRoot), global under globalDir.
func newProject(t *testing.T) (projectRoot, globalDir string) {
	t.Helper()
	return t.TempDir(), isolateUser(t)
}

// TestDiscover_AdaptsPromptAndShellSkills: project (and global) shell
// skills load like prompt-only ones — the hosting agent's bash gate,
// not the loader, decides what shell they may run. Only plugin shell
// skills carry a load-time trust gate (plugin_discovery_test.go).
func TestDiscover_AdaptsPromptAndShellSkills(t *testing.T) {
	projectRoot, globalDir := newProject(t)
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
	writeSkill(t, globalDir, "gscripted", `---
description: global shell skill.
allowed-tools: [Bash(git *)]
---
runs git
`)
	res, err := Discover(projectRoot)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(res.Tools) != 3 || len(res.Loaded) != 3 {
		t.Fatalf("expected all three skills loaded, got Loaded=%v", res.Loaded)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("project/global shell skills must not be skipped, got Skipped=%v", res.Skipped)
	}
	got := map[string]Source{}
	for _, s := range res.Loaded {
		got[s.Name] = s.Source
	}
	if got["scripted"] != SourceProject || got["gscripted"] != SourceGlobal || got["prompt-only"] != SourceProject {
		t.Fatalf("unexpected sources: %v", got)
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

// TestDiscover_SharedDirs: the cross-client .agents/skills directories
// load at both scopes, and nib's own directory wins a name clash within
// a scope.
func TestDiscover_SharedDirs(t *testing.T) {
	projectRoot, globalDir := newProject(t)
	sharedGlobal, ok := SharedGlobalDir()
	if !ok {
		t.Fatal("SharedGlobalDir unresolved")
	}
	writeSkill(t, SharedProjectDir(projectRoot), "shared-proj", "---\ndescription: shared project.\n---\nSP")
	writeSkill(t, sharedGlobal, "shared-glob", "---\ndescription: shared global.\n---\nSG")
	writeSkill(t, ProjectDir(projectRoot), "clash", "---\ndescription: nib project.\n---\nNP")
	writeSkill(t, SharedProjectDir(projectRoot), "clash", "---\ndescription: shared project.\n---\nSP")
	writeSkill(t, globalDir, "gclash", "---\ndescription: nib global.\n---\nNG")
	writeSkill(t, sharedGlobal, "gclash", "---\ndescription: shared global.\n---\nSG")

	res, err := Discover(projectRoot)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	got := loadedByName(res)
	if got["shared-proj"].Source != SourceProject || got["shared-glob"].Source != SourceGlobal {
		t.Errorf("shared skills not loaded at their scope: %v", got)
	}
	if got["clash"].Description != "nib project." || got["gclash"].Description != "nib global." {
		t.Errorf("nib's own directory must win a same-scope clash: %v", got)
	}
	if len(res.Shadowed) != 2 {
		t.Errorf("expected both shared clashes shadowed, got %v", res.Shadowed)
	}
}

// TestSharedProjectShadowsGlobal: project scope wins over user scope
// whichever directory convention each side uses.
func TestSharedProjectShadowsGlobal(t *testing.T) {
	projectRoot, globalDir := newProject(t)
	writeSkill(t, SharedProjectDir(projectRoot), "review", "---\ndescription: project.\n---\nP")
	writeSkill(t, globalDir, "review", "---\ndescription: global.\n---\nG")

	res, err := Discover(projectRoot)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(res.Loaded) != 1 || res.Loaded[0].Description != "project." {
		t.Errorf("project skill must shadow the global one, got %v", res.Loaded)
	}
}

// writeFile writes content at path, creating its parent directories.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// link creates a symlink at path naming target, or skips the test where
// symlinks are unavailable.
func link(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func loadedByName(res Result) map[string]Skill {
	got := map[string]Skill{}
	for _, s := range res.Loaded {
		got[s.Name] = s
	}
	return got
}

// loadOne writes a minimal skill under root and returns it as loaded.
func loadOne(t *testing.T, root, name string) Skill {
	t.Helper()
	writeSkill(t, root, name, "---\ndescription: d.\n---\nbody")
	skills, err := Load(root, SourceProject)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, s := range skills {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("skill %q not loaded from %s", name, root)
	return Skill{}
}

// bundle is loadOne plus one bundled reference file.
func bundle(t *testing.T, root, name string) Skill {
	t.Helper()
	writeFile(t, filepath.Join(root, name, "references", "forms.md"), "form notes")
	return loadOne(t, root, name)
}

func fileCall(file string) llm.ToolCall {
	return llm.ToolCall{Function: llm.FunctionCall{Arguments: `{"file":"` + file + `"}`}}
}

func TestResourceNote(t *testing.T) {
	root := t.TempDir()
	s := loadOne(t, root, "pdf")
	if note := s.ResourceNote("skill_pdf"); note != "" {
		t.Errorf("a skill that bundles nothing gets no note, got %q", note)
	}
	for _, f := range []string{"scripts/extract.py", "references/forms.md", ".git/config"} {
		writeFile(t, filepath.Join(root, "pdf", filepath.FromSlash(f)), "x")
	}
	note := s.ResourceNote("skill_pdf")
	for _, want := range []string{
		"Skill directory: " + s.Dir + "\n",
		`call skill_pdf with the "file" argument`,
		"<file>references/forms.md</file>",
		"<file>scripts/extract.py</file>",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q:\n%s", want, note)
		}
	}
	for _, reject := range []string{skillFile, ".git", "incomplete"} {
		if strings.Contains(note, reject) {
			t.Errorf("note must not mention %q:\n%s", reject, note)
		}
	}

	res := adaptTool(s).Execute(context.Background(), llm.ToolCall{})
	if res.Content != "body"+note {
		t.Errorf("Execute must append the note to the body, got %q", res.Content)
	}
}

func TestResourceNote_CapsListing(t *testing.T) {
	root := t.TempDir()
	s := loadOne(t, root, "big")
	for i := range maxListedResources + 1 {
		writeFile(t, filepath.Join(root, "big", "assets", fmt.Sprintf("f%03d.txt", i)), "")
	}
	note := s.ResourceNote("skill_big")
	if got := strings.Count(note, "<file>"); got != maxListedResources {
		t.Errorf("listed %d files, want the cap %d", got, maxListedResources)
	}
	if !strings.Contains(note, "listing incomplete") {
		t.Errorf("a capped listing must say so:\n%s", note)
	}
}

// TestBundled_BoundsWalk: a tree of many entries and few files stops at
// the walk budget and reports the listing as cut short.
func TestBundled_BoundsWalk(t *testing.T) {
	dir := t.TempDir()
	for i := range maxWalkedEntries + 1 {
		if err := os.Mkdir(filepath.Join(dir, fmt.Sprintf("d%04d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files, more, err := bundled(dir)
	if err != nil || len(files) != 0 || !more {
		t.Errorf("bundled = %v, more=%v, err=%v; want no files, more, no error", files, more, err)
	}
}

// TestResourceNote_UnreadableSubtree: a listing that failed part-way
// still names the directory and says it is incomplete, even with no
// file collected.
func TestResourceNote_UnreadableSubtree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	root := t.TempDir()
	s := loadOne(t, root, "locked")
	sub := filepath.Join(root, "locked", "assets")
	if err := os.Mkdir(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(sub, 0o755); err != nil {
			t.Errorf("restore %s: %v", sub, err)
		}
	})
	if _, err := os.ReadDir(sub); err == nil {
		t.Skip("permission bits are not enforced here")
	}
	note := s.ResourceNote("skill_locked")
	if !strings.Contains(note, "Skill directory: ") || !strings.Contains(note, "listing incomplete") {
		t.Errorf("a failed listing must keep the directory and the marker, got %q", note)
	}
}

func TestReadResource_ConfinedToSkillDir(t *testing.T) {
	root := t.TempDir()
	s := bundle(t, root, "pdf")
	writeFile(t, filepath.Join(root, "secret.txt"), "secret")
	link(t, filepath.Join(root, "secret.txt"), filepath.Join(root, "pdf", "leak.txt"))

	got, err := s.ReadResource("references/forms.md")
	if err != nil || got != "form notes" {
		t.Fatalf("ReadResource = %q, %v", got, err)
	}
	for name, why := range map[string]string{
		"../secret.txt": "a path leaving the skill directory",
		"references":    "a directory",
		"leak.txt":      "a symlink leaving the skill directory",
	} {
		if _, err := s.ReadResource(name); err == nil {
			t.Errorf("%s must be refused (%s)", why, name)
		}
	}
}

func TestReadResource_Cap(t *testing.T) {
	root := t.TempDir()
	s := bundle(t, root, "big")
	writeFile(t, filepath.Join(root, "big", "assets.bin"), strings.Repeat("x", maxResourceBytes+1))
	if _, err := s.ReadResource("assets.bin"); err == nil || !strings.Contains(err.Error(), "read cap") {
		t.Errorf("an oversize file must be refused with the cap named, got %v", err)
	}
}

// TestFileReaders: the skill's own tool and the standalone reader both
// return a bundled file and report an escaping path as a tool error.
func TestFileReaders(t *testing.T) {
	s := bundle(t, t.TempDir(), "pdf")
	own, reader := adaptTool(s), ResourceTool(s)
	if p := own.Definition().Function.Parameters; len(p.Properties) != 1 || len(p.Required) != 0 {
		t.Errorf("the skill tool takes one optional argument, got %+v", p)
	}
	def := reader.Definition().Function
	if def.Name != ResourceToolName || !slices.Equal(def.Parameters.Required, []string{"file"}) {
		t.Errorf("reader definition = %+v", def)
	}
	if strings.HasPrefix(def.Name, ToolNamePrefix) {
		t.Errorf("the reader must not look like a skill: %q", def.Name)
	}
	for name, tool := range map[string]kit.Tool{"own": own, "reader": reader} {
		if res := tool.Execute(context.Background(), fileCall("references/forms.md")); res.IsError || res.Content != "form notes" {
			t.Errorf("%s: file read = %+v", name, res)
		}
		if res := tool.Execute(context.Background(), fileCall("../x")); !res.IsError {
			t.Errorf("%s: an escaping path must be a tool error, got %+v", name, res)
		}
	}
	if res := reader.Execute(context.Background(), llm.ToolCall{}); !res.IsError {
		t.Errorf("the reader needs a file, got %+v", res)
	}
}

// TestDiscover_LinkedSkills: a symlinked skill directory loads under the
// link's name at project and user scope, and lists its target's files.
func TestDiscover_LinkedSkills(t *testing.T) {
	projectRoot, globalDir := newProject(t)
	inRepo := filepath.Join(projectRoot, "vendor-skills")
	bundle(t, inRepo, "real-proj")
	outside := t.TempDir()
	bundle(t, outside, "real-glob")
	link(t, filepath.Join(inRepo, "real-proj"), filepath.Join(SharedProjectDir(projectRoot), "linked-proj"))
	link(t, filepath.Join(outside, "real-glob"), filepath.Join(globalDir, "linked-glob"))

	res, err := Discover(projectRoot)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	got := loadedByName(res)
	if got["linked-proj"].Source != SourceProject || got["linked-glob"].Source != SourceGlobal {
		t.Fatalf("linked skills must load under the link's name: %v", loadedNames(res))
	}
	if note := got["linked-glob"].ResourceNote("t"); !strings.Contains(note, "<file>references/forms.md</file>") {
		t.Errorf("a linked skill must list its target's files:\n%s", note)
	}
}

// TestDiscover_ProjectLinkOutsideRefused: a project skill link that
// names a directory outside the project is refused and surfaced.
func TestDiscover_ProjectLinkOutsideRefused(t *testing.T) {
	projectRoot, _ := newProject(t)
	outside := t.TempDir()
	bundle(t, outside, "elsewhere")
	link(t, filepath.Join(outside, "elsewhere"), filepath.Join(SharedProjectDir(projectRoot), "sneaky"))

	res, err := Discover(projectRoot)
	if err != nil {
		t.Fatalf("a refusal is not a load error: %v", err)
	}
	if len(res.Loaded) != 0 || len(res.Refused) != 1 || res.Refused[0].Name != "sneaky" {
		t.Fatalf("Loaded=%v Refused=%v, want only sneaky refused", loadedNames(res), res.Refused)
	}
	if out := kit.RenderPlugins(Plugins(res)); !strings.Contains(out, "links outside the project") {
		t.Errorf("the refusal must show in the plugin manifest:\n%s", out)
	}
}

func TestLoad_DanglingLinkReported(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "ok", "---\ndescription: d.\n---\nbody")
	link(t, filepath.Join(root, "missing"), filepath.Join(root, "dangling"))
	skills, err := Load(root, SourceGlobal)
	if err == nil || !strings.Contains(err.Error(), "dangling") {
		t.Errorf("a dangling link must be reported, got %v", err)
	}
	if len(skills) != 1 || skills[0].Name != "ok" {
		t.Errorf("the other skill must still load, got %v", skills)
	}
}
