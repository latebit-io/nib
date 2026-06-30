package skill

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/ai/brand"
)

func writeSkillFile(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func loadedNames(res Result) []string {
	var names []string
	for _, s := range res.Loaded {
		names = append(names, s.Name)
	}
	slices.Sort(names)
	return names
}

// TestSkill_PermissionsFromGrants checks allowed/disallowed-tools are
// parsed and compile into a working matcher (deny wins).
func TestSkill_PermissionsFromGrants(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, dir, "runner",
		"---\ndescription: runs git\nallowed-tools: Bash(git *)\ndisallowed-tools: Bash(git push *)\n---\nbody\n")

	skills, err := Load(dir, SourcePlugin)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("got %d skills", len(skills))
	}
	s := skills[0]
	if len(s.AllowedTools) != 1 || len(s.DisallowedTools) != 1 {
		t.Fatalf("grants not parsed: allowed=%v disallowed=%v", s.AllowedTools, s.DisallowedTools)
	}
	perm := s.Permissions()
	if !perm.Allows("Bash", "git status") {
		t.Errorf("git status should be allowed")
	}
	if perm.Allows("Bash", "git push origin main") {
		t.Errorf("git push should be denied (disallowed wins)")
	}
}

func TestSkill_PermissionsFailClosed(t *testing.T) {
	t.Parallel()
	// A broad allow plus a malformed deny must NOT leave the allow active.
	s := Skill{
		AllowedTools:    []string{"Bash(*)"},
		DisallowedTools: []string{"Bash(rm *"}, // unbalanced paren
	}
	if s.Permissions().Allows("Bash", "rm -rf /") {
		t.Errorf("malformed deny must fail closed, not drop and let the allow win")
	}
}

func TestSkill_LoadRejectsMalformedGrant(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeSkillFile(t, dir, "bad",
		"---\ndescription: d\nallowed-tools: Bash(*)\ndisallowed-tools: Bash(rm *\n---\nbody\n")
	_, err := Load(dir, SourcePlugin)
	if err == nil {
		t.Errorf("expected load error for malformed grant")
	}
}

func TestDiscoverWithPlugins_EmptyIDNotTrusted(t *testing.T) {
	t.Setenv(brand.EnvKeyGlobalSkillsDir, t.TempDir())
	pluginDir := t.TempDir()
	writeSkillFile(t, pluginDir, "runner",
		"---\ndescription: runs\nallowed-tools: Bash(git *)\n---\nrun\n")
	// Empty ID + trust-everything must still NOT load the shell skill.
	res, err := DiscoverWithPlugins(t.TempDir(),
		[]PluginSkillSource{{ID: "", Dir: pluginDir}}, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(loadedNames(res), "runner") {
		t.Errorf("empty-id plugin source must not be trusted: %v", loadedNames(res))
	}
}

func TestSkill_LoadFork(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeSkillFile(t, dir, "forker", "---\ndescription: forks\ncontext: fork\n---\ndo work\n")
	skills, err := Load(dir, SourcePlugin)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || !skills[0].IsFork() {
		t.Fatalf("fork frontmatter not parsed: %+v", skills)
	}

	// Invalid context value is rejected.
	writeSkillFile(t, dir, "forker", "---\ndescription: d\ncontext: spoon\n---\nbody\n")
	if _, err := Load(dir, SourcePlugin); err == nil {
		t.Errorf("invalid context value should error")
	}

	// `agent:` references are not yet supported — rejected at load.
	writeSkillFile(t, dir, "forker", "---\ndescription: d\ncontext: fork\nagent: reviewer\n---\nbody\n")
	if _, err := Load(dir, SourcePlugin); err == nil {
		t.Errorf("agent: reference should be rejected")
	}
}

func hasSkill(ss []Skill, name string) bool {
	return slices.ContainsFunc(ss, func(s Skill) bool { return s.Name == name })
}

func hasToolNamed(tools []upagent.Tool, name string) bool {
	return slices.ContainsFunc(tools, func(tl upagent.Tool) bool { return tl.Definition().Function.Name == name })
}

func TestDiscoverWithPlugins_ForkRouting(t *testing.T) {
	t.Setenv(brand.EnvKeyGlobalSkillsDir, t.TempDir())
	projectRoot := t.TempDir()
	writeSkillFile(t, ProjectDir(projectRoot), "forker", "---\ndescription: forks\ncontext: fork\n---\nwork\n")
	pluginDir := t.TempDir()
	writeSkillFile(t, pluginDir, "pforker", "---\ndescription: p forks\ncontext: fork\n---\nwork\n")
	srcs := []PluginSkillSource{{ID: "demo", Dir: pluginDir}}

	// Project fork → Forking (not a prompt tool). Untrusted plugin fork → Skipped.
	un, err := DiscoverWithPlugins(projectRoot, srcs, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if !hasSkill(un.Forking, "forker") {
		t.Errorf("project fork skill should be routed to Forking")
	}
	if hasToolNamed(un.Tools, "skill_forker") {
		t.Errorf("fork skill must not be adapted to a prompt tool")
	}
	if !hasSkill(un.Skipped, "pforker") {
		t.Errorf("untrusted plugin fork skill must be skipped")
	}

	// Trusted plugin fork → Forking.
	tr, err := DiscoverWithPlugins(projectRoot, srcs, func(id string) bool { return id == "demo" })
	if err != nil {
		t.Fatal(err)
	}
	if !hasSkill(tr.Forking, "pforker") {
		t.Errorf("trusted plugin fork skill should be in Forking")
	}
}

// TestDiscoverWithPlugins_TrustGate covers the shell trust gate: a
// plugin's shell-bearing skill loads only when its plugin is trusted;
// untrusted it is refused, and a non-shell plugin skill always loads.
func TestDiscoverWithPlugins_TrustGate(t *testing.T) {
	t.Setenv(brand.EnvKeyGlobalSkillsDir, t.TempDir())
	projectRoot := t.TempDir()

	pluginDir := t.TempDir()
	writeSkillFile(t, pluginDir, "runner",
		"---\ndescription: runs git\nallowed-tools: Bash(git *)\n---\nrun\n")
	writeSkillFile(t, pluginDir, "helper",
		"---\ndescription: prompt only\n---\nhelp\n")
	srcs := []PluginSkillSource{{ID: "demo", Dir: pluginDir}}

	// Untrusted: the shell skill is refused, the prompt skill loads.
	untrusted, err := DiscoverWithPlugins(projectRoot, srcs, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(loadedNames(untrusted), "runner") {
		t.Errorf("untrusted plugin shell skill must not load: %v", loadedNames(untrusted))
	}
	if !slices.Contains(loadedNames(untrusted), "helper") {
		t.Errorf("prompt-only plugin skill should load regardless of trust")
	}
	if !slices.ContainsFunc(untrusted.Skipped, func(s Skill) bool { return s.Name == "runner" }) {
		t.Errorf("refused shell skill should be in Skipped")
	}

	// Trusted: the shell skill loads, with provenance stamped.
	trusted, err := DiscoverWithPlugins(projectRoot, srcs, func(id string) bool { return id == "demo" })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(loadedNames(trusted), "runner") {
		t.Errorf("trusted plugin shell skill must load: %v", loadedNames(trusted))
	}
	for _, s := range trusted.Loaded {
		if s.Name == "runner" && s.PluginID != "demo" {
			t.Errorf("PluginID provenance not stamped: %q", s.PluginID)
		}
	}
}

// TestDiscoverWithPlugins_LoadsAndShadows verifies plugin skills load as
// the lowest layer and a same-named project skill shadows the plugin one.
func TestDiscoverWithPlugins_LoadsAndShadows(t *testing.T) {
	// Isolate the global layer so the host's real skills don't leak in.
	t.Setenv(brand.EnvKeyGlobalSkillsDir, t.TempDir())

	projectRoot := t.TempDir()
	writeSkillFile(t, ProjectDir(projectRoot), "shared", "---\ndescription: project version\n---\nproject body\n")
	writeSkillFile(t, ProjectDir(projectRoot), "projonly", "---\ndescription: p\n---\nbody\n")

	pluginDir := t.TempDir()
	writeSkillFile(t, pluginDir, "shared", "---\ndescription: plugin version\n---\nplugin body\n")
	writeSkillFile(t, pluginDir, "plugonly", "---\ndescription: pg\n---\nbody\n")

	res, err := DiscoverWithPlugins(projectRoot, []PluginSkillSource{{ID: "p", Dir: pluginDir}}, nil)
	if err != nil {
		t.Fatalf("DiscoverWithPlugins: %v", err)
	}

	if got := loadedNames(res); !slices.Equal(got, []string{"plugonly", "projonly", "shared"}) {
		t.Errorf("loaded = %v", got)
	}
	// The surviving "shared" must be the project one (project shadows plugin).
	for _, s := range res.Loaded {
		if s.Name == "shared" && s.Source != SourceProject {
			t.Errorf("shared skill source = %q, want project (project must shadow plugin)", s.Source)
		}
	}
	// The shadowed plugin skill is surfaced, not silently dropped.
	if !slices.ContainsFunc(res.Shadowed, func(s Skill) bool {
		return s.Name == "shared" && s.Source == SourcePlugin
	}) {
		t.Errorf("expected shadowed plugin 'shared', got %+v", res.Shadowed)
	}
}
