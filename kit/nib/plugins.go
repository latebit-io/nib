package nib

import (
	"fmt"
	"slices"
	"strings"

	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/mcp"
	"github.com/latebit-io/nib/kit/pluginstore"
	"github.com/latebit-io/nib/kit/skill"
	"github.com/latebit-io/nib/kit/subagent"
)

// installed is what a session takes from the installed plugins: their
// skills always, their MCP servers, hooks and agents only once trusted,
// since those run code.
type installed struct {
	active []pluginstore.ActivePlugin
}

// trusted reports whether the person trusts the plugin with id.
func (p installed) trusted(id string) bool {
	i := slices.IndexFunc(p.active, func(a pluginstore.ActivePlugin) bool { return a.ID == id })
	return i >= 0 && p.active[i].Trusted
}

// agentDirs are the trusted plugins' agent directories.
func (p installed) agentDirs() []subagent.PluginDir {
	var out []subagent.PluginDir
	for _, a := range p.active {
		if a.Trusted {
			out = append(out, subagent.PluginDir{ID: a.ID, Dir: a.AgentsDir})
		}
	}
	return out
}

// skillSources are the plugins' skill directories.
func (p installed) skillSources() []skill.PluginSkillSource {
	out := make([]skill.PluginSkillSource, 0, len(p.active))
	for _, a := range p.active {
		out = append(out, skill.PluginSkillSource{ID: a.ID, Dir: a.SkillsDir})
	}
	return out
}

// mcpSources are the trusted plugins' MCP config files.
func (p installed) mcpSources() []mcp.ConfigSource {
	var out []mcp.ConfigSource
	for _, a := range p.active {
		if a.Trusted {
			out = append(out, mcp.ConfigSource{Prefix: a.ID, Path: a.MCPConfigPath})
		}
	}
	return out
}

// manifest lists the enabled plugins and whether each is trusted.
func (p installed) manifest() []kit.Plugin {
	rows := make([]kit.Plugin, 0, len(p.active))
	for _, a := range p.active {
		desc := "trusted: its hooks, MCP servers and agents run"
		if !a.Trusted {
			desc = "not trusted: skills only; hooks, MCP servers and agents stay off"
		}
		rows = append(rows, kit.Plugin{Kind: kit.KindPlugin, Name: a.ID, Description: desc})
	}
	return rows
}

// untrustedNote is one warning for the enabled plugins nobody trusted.
func (p installed) untrustedNote() error {
	var ids []string
	for _, a := range p.active {
		if !a.Trusted {
			ids = append(ids, a.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return fmt.Errorf("plugin(s) %s not trusted: only their skills load", strings.Join(ids, ", "))
}
