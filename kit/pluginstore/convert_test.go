package pluginstore

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildDemoPlugin writes a fixture CC plugin exercising every component
// the M1 converter handles plus the ones it defers.
func buildDemoPlugin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".claude-plugin", "plugin.json"), `{"name":"demo","version":"1.0.0"}`)
	writeFile(t, filepath.Join(root, "commands", "greet.md"),
		"---\ndescription: Greet someone\n---\nHello $ARGUMENTS from ${CLAUDE_PLUGIN_ROOT}\n")
	writeFile(t, filepath.Join(root, "skills", "helper", "SKILL.md"),
		"---\ndescription: A prompt-only helper\n---\nGuidance body.\n")
	writeFile(t, filepath.Join(root, "skills", "helper", "reference.md"), "extra ref\n")
	writeFile(t, filepath.Join(root, "skills", "runner", "SKILL.md"),
		"---\ndescription: runs things\nallowed-tools: Bash(git *)\n---\nrun it\n")
	writeFile(t, filepath.Join(root, ".mcp.json"),
		`{"mcpServers":{"db":{"command":"${CLAUDE_PLUGIN_ROOT}/bin/db","args":["--data","${CLAUDE_PLUGIN_DATA}"]},"remote":{"type":"sse","url":"https://x/mcp"}}}`)
	writeFile(t, filepath.Join(root, "agents", "reviewer.md"),
		"---\nname: reviewer\ndescription: reviews code\ntools: Read Grep\n---\nReview the code in ${CLAUDE_PLUGIN_ROOT}.\n")
	writeFile(t, filepath.Join(root, "hooks", "hooks.json"), `{"hooks":{}}`)
	return root
}

func TestConvert_FullPlugin(t *testing.T) {
	t.Parallel()
	src := buildDemoPlugin(t)
	dst := t.TempDir()
	vars := Vars{PluginRoot: "/ROOT", PluginData: "/DATA"}

	report, err := Convert(src, dst, Manifest{Name: "demo", Version: "1.0.0"}, vars)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	if !slices.Equal(report.Commands, []string{"demo-greet"}) {
		t.Errorf("commands = %v", report.Commands)
	}
	// Shell-bearing skills are now converted too (grants preserved), so
	// both helper and runner appear.
	if !slices.Equal(report.Skills, []string{"demo-helper", "demo-runner"}) {
		t.Errorf("skills = %v", report.Skills)
	}
	if !slices.Equal(report.MCPServers, []string{"db"}) {
		t.Errorf("mcp = %v", report.MCPServers)
	}

	// The agent converts now (namespaced), with its var frozen.
	if !slices.Equal(report.Agents, []string{"demo-reviewer"}) {
		t.Errorf("agents = %v", report.Agents)
	}
	agentMD, err := os.ReadFile(filepath.Join(dst, "agents", "demo-reviewer.md"))
	if err != nil {
		t.Fatalf("converted agent missing: %v", err)
	}
	if !strings.Contains(string(agentMD), "name: demo-reviewer") || !strings.Contains(string(agentMD), "Review the code in /ROOT.") {
		t.Errorf("converted agent wrong:\n%s", agentMD)
	}

	// Deferred/unsupported components are reported, not dropped silently.
	wantUnsupported := map[string]string{
		"skill-shell":   "demo-runner",
		"mcp-transport": "remote",
	}
	for kind, name := range wantUnsupported {
		if !slices.ContainsFunc(report.Unsupported, func(u Unsupported) bool {
			return u.Kind == kind && u.Name == name
		}) {
			t.Errorf("missing unsupported %s/%s in %+v", kind, name, report.Unsupported)
		}
	}

	// Command converted: namespaced name, var frozen in body.
	cmd, err := os.ReadFile(filepath.Join(dst, "commands", "demo-greet.md"))
	if err != nil {
		t.Fatalf("read converted command: %v", err)
	}
	if !strings.Contains(string(cmd), "name: demo-greet") {
		t.Errorf("command frontmatter missing namespaced name:\n%s", cmd)
	}
	if !strings.Contains(string(cmd), "Hello $ARGUMENTS from /ROOT") {
		t.Errorf("command body var not frozen:\n%s", cmd)
	}

	// Prompt skill copied whole (reference file too) with namespaced name.
	if _, err := os.Stat(filepath.Join(dst, "skills", "demo-helper", "reference.md")); err != nil {
		t.Errorf("skill reference file not copied: %v", err)
	}
	skill, _ := os.ReadFile(filepath.Join(dst, "skills", "demo-helper", "SKILL.md"))
	if !strings.Contains(string(skill), "name: demo-helper") {
		t.Errorf("skill frontmatter missing namespaced name:\n%s", skill)
	}
	// Shell skill IS converted now, with its grant preserved in frontmatter.
	runner, err := os.ReadFile(filepath.Join(dst, "skills", "demo-runner", "SKILL.md"))
	if err != nil {
		t.Fatalf("shell-bearing skill should be converted: %v", err)
	}
	if !strings.Contains(string(runner), "allowed-tools:") || !strings.Contains(string(runner), "Bash(git *)") {
		t.Errorf("shell skill grant not carried into frontmatter:\n%s", runner)
	}

	// MCP converted: stdio kept + frozen, sse dropped.
	mcp, err := os.ReadFile(filepath.Join(dst, ".mcp.json"))
	if err != nil {
		t.Fatalf("read converted mcp: %v", err)
	}
	if !strings.Contains(string(mcp), `/ROOT/bin/db`) || !strings.Contains(string(mcp), `/DATA`) {
		t.Errorf("mcp vars not frozen:\n%s", mcp)
	}
	if strings.Contains(string(mcp), "remote") {
		t.Errorf("sse server should be dropped:\n%s", mcp)
	}
}

func TestConvert_NameCollisionReported(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, ".claude-plugin", "plugin.json"), `{"name":"demo"}`)
	// Two command files that sanitize to the same namespaced name.
	writeFile(t, filepath.Join(src, "commands", "foo bar.md"), "---\ndescription: a\n---\nA\n")
	writeFile(t, filepath.Join(src, "commands", "foo-bar.md"), "---\ndescription: b\n---\nB\n")

	report, err := Convert(src, t.TempDir(), Manifest{Name: "demo"}, Vars{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	// Exactly one survives; the collision is reported, not silently dropped.
	if len(report.Commands) != 1 {
		t.Errorf("expected 1 converted command, got %v", report.Commands)
	}
	if !slices.ContainsFunc(report.Unsupported, func(u Unsupported) bool {
		return u.Kind == "command" && strings.Contains(u.Reason, "collides")
	}) {
		t.Errorf("expected a collision report entry, got %+v", report.Unsupported)
	}
}

func TestConvert_CarriesCommandGrants(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, ".claude-plugin", "plugin.json"), `{"name":"demo"}`)
	writeFile(t, filepath.Join(src, "commands", "deploy.md"),
		"---\ndescription: ship\nallowed-tools: Bash(git *) Read\ndisallowed-tools: Bash(rm *)\n---\nDeploy.\n")

	dst := t.TempDir()
	if _, err := Convert(src, dst, Manifest{Name: "demo"}, Vars{}); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	out, err := os.ReadFile(filepath.Join(dst, "commands", "demo-deploy.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"allowed-tools:", "Bash(git *)", "Read", "disallowed-tools:", "Bash(rm *)"} {
		if !strings.Contains(s, want) {
			t.Errorf("converted command missing %q:\n%s", want, s)
		}
	}
}

func TestConvert_SkipsMalformedGrant(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, ".claude-plugin", "plugin.json"), `{"name":"demo"}`)
	// Broad allow + malformed deny: must be skipped, not converted with the
	// deny silently dropped.
	writeFile(t, filepath.Join(src, "commands", "danger.md"),
		"---\ndescription: x\nallowed-tools: Bash(*)\ndisallowed-tools: Bash(rm *\n---\nbody\n")

	dst := t.TempDir()
	report, err := Convert(src, dst, Manifest{Name: "demo"}, Vars{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if len(report.Commands) != 0 {
		t.Errorf("malformed-grant command should be skipped, got %v", report.Commands)
	}
	if !slices.ContainsFunc(report.Unsupported, func(u Unsupported) bool {
		return u.Kind == "command" && strings.Contains(u.Reason, "malformed")
	}) {
		t.Errorf("expected malformed-grant report entry, got %+v", report.Unsupported)
	}
	if _, err := os.Stat(filepath.Join(dst, "commands", "demo-danger.md")); !os.IsNotExist(err) {
		t.Errorf("malformed-grant artifact must not be written")
	}
}

func TestConvert_AgentSkillRefsNamespaced(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, ".claude-plugin", "plugin.json"), `{"name":"demo"}`)
	writeFile(t, filepath.Join(src, "agents", "rev.md"),
		"---\nname: rev\ndescription: d\nskills: [runner, helper]\n---\nReview.\n")

	dst := t.TempDir()
	if _, err := Convert(src, dst, Manifest{Name: "demo"}, Vars{}); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(filepath.Join(dst, "agents", "demo-rev.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"demo-runner", "demo-helper"} {
		if !strings.Contains(s, want) {
			t.Errorf("agent skill ref not namespaced to %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "- runner\n") || strings.Contains(s, "- helper\n") {
		t.Errorf("bare (un-namespaced) skill ref leaked:\n%s", s)
	}
}

func TestConvert_Hooks(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, ".claude-plugin", "plugin.json"), `{"name":"demo"}`)
	writeFile(t, filepath.Join(src, "hooks", "hooks.json"), `{
      "hooks": {
        "PostToolUse": [{"matcher":"Write|Edit","hooks":[{"type":"command","command":"${CLAUDE_PLUGIN_ROOT}/fmt.sh"}]}],
        "SessionStart": [{"hooks":[{"type":"http","url":"https://x/hook"}]}],
        "Bogus": [{"hooks":[{"type":"command","command":"x.sh"}]}]
      }
    }`)

	dst := t.TempDir()
	report, err := Convert(src, dst, Manifest{Name: "demo"}, Vars{PluginRoot: "/ROOT"})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	// Only known events with a runnable command hook are listed.
	if !slices.Equal(report.Hooks, []string{"PostToolUse"}) {
		t.Errorf("report.Hooks = %v", report.Hooks)
	}
	// Converted config written with the var frozen.
	out, err := os.ReadFile(filepath.Join(dst, "hooks", "hooks.json"))
	if err != nil {
		t.Fatalf("converted hooks missing: %v", err)
	}
	if !strings.Contains(string(out), "/ROOT/fmt.sh") {
		t.Errorf("hook command var not frozen:\n%s", out)
	}
	// http hook + unknown event reported, not silently dropped.
	wantUnsupported := map[string]string{"SessionStart": `"http" hook type not yet runnable`, "Bogus": "unrecognized event"}
	for ev, reason := range wantUnsupported {
		if !slices.ContainsFunc(report.Unsupported, func(u Unsupported) bool {
			return u.Kind == "hook" && u.Name == ev && u.Reason == reason
		}) {
			t.Errorf("missing unsupported hook %s/%s in %+v", ev, reason, report.Unsupported)
		}
	}
}

func TestConvert_MalformedHooksReported(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, ".claude-plugin", "plugin.json"), `{"name":"demo"}`)
	writeFile(t, filepath.Join(src, "hooks", "hooks.json"), `{not json`)

	report, err := Convert(src, t.TempDir(), Manifest{Name: "demo"}, Vars{})
	if err != nil {
		t.Fatalf("Convert should not fail on malformed hooks: %v", err)
	}
	if !slices.ContainsFunc(report.Unsupported, func(u Unsupported) bool {
		return u.Kind == "hooks" && strings.Contains(u.Reason, "malformed")
	}) {
		t.Errorf("malformed hooks should be reported, got %+v", report.Unsupported)
	}
}

func TestExpandVars(t *testing.T) {
	t.Parallel()
	v := Vars{PluginRoot: "/r", PluginData: "/d"}
	got, un := expandVars("${CLAUDE_PLUGIN_ROOT}/x ${user_config.tok} ${CLAUDE_PLUGIN_DATA}", v)
	if got != "/r/x ${user_config.tok} /d" {
		t.Errorf("expand = %q", got)
	}
	if !slices.Equal(un, []string{"user_config.tok"}) {
		t.Errorf("unresolved = %v", un)
	}
}

func TestStore_InstallConverts(t *testing.T) {
	t.Parallel()
	src := buildDemoPlugin(t)
	st, err := New(t.TempDir(), WithFetcher(&fakeFetcher{srcDir: src, pin: "sha1"}))
	if err != nil {
		t.Fatal(err)
	}
	ent, err := st.Install(context.Background(), LocalSource(src), InstallOptions{Enabled: true})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	// Converted artifacts produced.
	if _, err := os.Stat(filepath.Join(st.ConvertedDir(ent.ID), ".mcp.json")); err != nil {
		t.Errorf("converted mcp missing: %v", err)
	}
	// Report persisted and readable.
	report, err := st.ImportReport(ent.ID)
	if err != nil {
		t.Fatalf("ImportReport: %v", err)
	}
	if !slices.Equal(report.Commands, []string{"demo-greet"}) {
		t.Errorf("report.Commands = %v", report.Commands)
	}
	if len(report.Unsupported) == 0 {
		t.Errorf("expected unsupported entries in report")
	}
}
