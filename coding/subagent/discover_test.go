package subagent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/kit/agentdef"
)

func writeAgentFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func loadedAgentNames(res DiscoverResult) []string {
	var n []string
	for _, d := range res.Loaded {
		n = append(n, d.Name)
	}
	slices.Sort(n)
	return n
}

func toolNamesOf(res DiscoverResult) []string {
	var n []string
	for _, tl := range res.Tools {
		n = append(n, tl.Definition().Function.Name)
	}
	slices.Sort(n)
	return n
}

func TestDiscover_LayersTrustAndShadow(t *testing.T) {
	t.Setenv(brand.EnvKeyGlobalAgentsDir, t.TempDir())
	projectRoot := t.TempDir()
	spawner := &Spawner{} // not run in this test

	// Project agent (always allowed) + a name shared with a plugin.
	writeAgentFile(t, ProjectAgentsDir(projectRoot), "shared.md", "---\ndescription: project\n---\nproj prompt\n")
	writeAgentFile(t, ProjectAgentsDir(projectRoot), "projonly.md", "---\ndescription: p\n---\nprompt\n")

	pluginDir := t.TempDir()
	writeAgentFile(t, pluginDir, "shared.md", "---\ndescription: plugin\n---\nplugin prompt\n")
	writeAgentFile(t, pluginDir, "reviewer.md", "---\ndescription: r\n---\nreview prompt\n")
	srcs := []PluginAgentSource{{ID: "demo", Dir: pluginDir}}

	// Untrusted: plugin agents refused; project agents load.
	un, err := Discover(projectRoot, srcs, func(string) bool { return false }, spawner)
	if err != nil {
		t.Fatal(err)
	}
	if got := loadedAgentNames(un); !slices.Equal(got, []string{"projonly", "shared"}) {
		t.Errorf("untrusted loaded = %v", got)
	}
	if !slices.ContainsFunc(un.Skipped, func(d agentdef.Definition) bool { return d.Name == "reviewer" }) {
		t.Errorf("untrusted plugin agent should be skipped")
	}
	// 'shared' from the plugin is shadowed by the project one.
	if !slices.ContainsFunc(un.Shadowed, func(d agentdef.Definition) bool {
		return d.Name == "shared" && d.Source == agentdef.SourcePlugin
	}) {
		t.Errorf("plugin 'shared' should be shadowed by project")
	}

	// Trusted: plugin reviewer now loads and is adapted to agent_reviewer.
	tr, err := Discover(projectRoot, srcs, func(id string) bool { return id == "demo" }, spawner)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(loadedAgentNames(tr), "reviewer") {
		t.Errorf("trusted plugin agent should load: %v", loadedAgentNames(tr))
	}
	if !slices.Contains(toolNamesOf(tr), "agent_reviewer") {
		t.Errorf("expected agent_reviewer tool, got %v", toolNamesOf(tr))
	}
	// The shadowed plugin 'shared' must still defer to the project one.
	for _, d := range tr.Loaded {
		if d.Name == "shared" && d.Source != agentdef.SourceProject {
			t.Errorf("project 'shared' must win, got source %q", d.Source)
		}
	}
}

func TestDiscover_EmptyIDNotTrusted(t *testing.T) {
	t.Setenv(brand.EnvKeyGlobalAgentsDir, t.TempDir())
	pluginDir := t.TempDir()
	writeAgentFile(t, pluginDir, "x.md", "---\ndescription: d\n---\nprompt\n")
	res, err := Discover(t.TempDir(), []PluginAgentSource{{ID: "", Dir: pluginDir}}, func(string) bool { return true }, &Spawner{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Loaded) != 0 {
		t.Errorf("empty-id plugin source must contribute nothing, got %v", loadedAgentNames(res))
	}
}
