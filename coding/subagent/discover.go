package subagent

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/coding/agent"
	"github.com/latebit-io/nib/kit/agentdef"
)

// PluginAgentSource locates one enabled plugin's converted agents
// directory with the plugin id the trust gate keys on.
type PluginAgentSource struct {
	// ID is the managed-plugin id (stamped onto each loaded definition).
	ID string
	// Dir is the converted agents root (<converted>/agents).
	Dir string
}

// TrustFunc reports whether a managed plugin is trusted. A nil TrustFunc
// trusts nothing.
type TrustFunc func(pluginID string) bool

// DiscoverResult is the outcome of [Discover].
type DiscoverResult struct {
	// Tools are the adapted spawn tools, ready to pass to the parent
	// agent. Loaded[i] corresponds to Tools[i].
	Tools []agent.Tool
	// Loaded are the definitions adapted into Tools.
	Loaded []agentdef.Definition
	// Skipped are untrusted plugin definitions, refused with a logged
	// reason and surfaced here rather than dropped silently.
	Skipped []agentdef.Definition
	// Shadowed are definitions hidden by a higher-precedence same-name
	// definition.
	Shadowed []agentdef.Definition
}

// ProjectAgentsDir is the project-local subagent directory.
func ProjectAgentsDir(projectRoot string) string {
	return filepath.Join(projectRoot, ".project", "agents")
}

// GlobalAgentsDir returns the user-global subagent directory and true, or
// ("", false) when it cannot be resolved. [brand.EnvKeyGlobalAgentsDir]
// overrides the default <UserConfigDir>/<ConfigDirName>/agents.
func GlobalAgentsDir() (string, bool) {
	if dir := os.Getenv(brand.EnvKeyGlobalAgentsDir); dir != "" {
		return dir, true
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		slog.Warn("subagent: cannot resolve user config dir; skipping global agents layer", "err", err)
		return "", false
	}
	return filepath.Join(cfg, brand.ConfigDirName, "agents"), true
}

// Discover loads subagent definitions from the project, user-global, and
// enabled-plugin layers, applies precedence (project > global > plugin),
// gates plugin definitions behind trust, and adapts the survivors into
// spawn tools bound to spawner.
//
// Trust for agents is stricter than for skills: ANY plugin agent is
// refused unless its plugin is trusted, because a subagent runs a full
// child agent (tools, edits) — not merely injected prompt text. Project
// and global agents are user-authored and always adapted. A nil/empty
// plugins slice + nil trusted yields just the project/global agents.
func Discover(projectRoot string, plugins []PluginAgentSource, trusted TrustFunc, spawner *Spawner) (DiscoverResult, error) {
	var errs []error

	project, err := agentdef.LoadDir(ProjectAgentsDir(projectRoot), agentdef.SourceProject)
	if err != nil {
		errs = append(errs, err)
	}

	var global []agentdef.Definition
	if gdir, ok := GlobalAgentsDir(); ok {
		global, err = agentdef.LoadDir(gdir, agentdef.SourceGlobal)
		if err != nil {
			errs = append(errs, err)
		}
	}

	var plugin []agentdef.Definition
	for _, src := range plugins {
		if src.ID == "" {
			slog.Warn("subagent: skipping plugin agents source with empty id", "dir", src.Dir)
			continue
		}
		ds, derr := agentdef.LoadDir(src.Dir, agentdef.SourcePlugin)
		if derr != nil {
			errs = append(errs, derr)
		}
		for i := range ds {
			ds[i].PluginID = src.ID
		}
		plugin = append(plugin, ds...)
	}

	winners, shadowed := mergeDefs(project, global, plugin)

	var res DiscoverResult
	res.Shadowed = shadowed
	for _, d := range shadowed {
		slog.Info("subagent: shadowed by higher-precedence definition", "agent", d.Name, "source", d.Source, "path", d.Path)
	}
	for _, d := range winners {
		if d.Source == agentdef.SourcePlugin && !pluginTrusted(d, trusted) {
			res.Skipped = append(res.Skipped, d)
			slog.Warn("subagent: refused untrusted plugin agent (run /plugin trust to enable)",
				"agent", d.Name, "plugin", d.PluginID, "path", d.Path)
			continue
		}
		res.Tools = append(res.Tools, AdaptTool(d, spawner))
		res.Loaded = append(res.Loaded, d)
	}

	if len(errs) > 0 {
		return res, fmt.Errorf("load agents: %w", errors.Join(errs...))
	}
	return res, nil
}

// pluginTrusted reports whether a plugin definition may be adapted: a
// non-empty plugin id whose plugin the user has trusted.
func pluginTrusted(d agentdef.Definition, trusted TrustFunc) bool {
	return d.PluginID != "" && trusted != nil && trusted(d.PluginID)
}

// mergeDefs deduplicates definitions by name across precedence layers
// (earliest layer wins); later same-name definitions are returned as
// shadowed.
func mergeDefs(layers ...[]agentdef.Definition) (winners, shadowed []agentdef.Definition) {
	seen := map[string]bool{}
	for _, layer := range layers {
		for _, d := range layer {
			if seen[d.Name] {
				shadowed = append(shadowed, d)
				continue
			}
			seen[d.Name] = true
			winners = append(winners, d)
		}
	}
	return winners, shadowed
}
