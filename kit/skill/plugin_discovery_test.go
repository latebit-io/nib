package skill

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

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

	res, err := DiscoverWithPlugins(projectRoot, []string{pluginDir})
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
