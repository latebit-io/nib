package main

import (
	"github.com/latebit-io/nib/ai/llmconfig"
	"github.com/latebit-io/nib/coding/agent"
	"github.com/latebit-io/nib/kit"
	kitcmd "github.com/latebit-io/nib/kit/command"
	"github.com/latebit-io/nib/kit/memory"
	"github.com/latebit-io/nib/kit/skill"
)

// buildPluginsManifest assembles the `--plugins` output for nib-code.
// Walks the parts the binary holds — provider, store, agent's tool
// snapshot, command registry — into a []kit.Plugin slice and renders
// via [kit.RenderPlugins].
//
// Each section is skipped gracefully when the corresponding component
// is absent (no provider configured, no agent built, etc.) so the
// command stays useful before LLM credentials are wired.
func buildPluginsManifest(
	ag *agent.Agent,
	registry *kitcmd.Registry,
	store memory.Store,
	resolved *llmconfig.Resolved,
	skippedSkills []string,
) string {
	var plugins []kit.Plugin

	if resolved != nil && resolved.HasProvider() {
		plugins = append(plugins, kit.Plugin{
			Kind:        kit.KindProvider,
			Name:        resolved.Profile,
			Description: resolved.DisplayModel(),
		})
	}

	if store != nil {
		plugins = append(plugins, kit.Plugin{
			Kind:        kit.KindStore,
			Name:        "demarkus",
			Description: "Mark Protocol versioned memory",
		})
	}

	if ag != nil {
		for _, t := range ag.Tools() {
			plugins = append(plugins, kit.DescribeTool(t))
		}
	}

	if registry != nil {
		for _, c := range registry.List() {
			plugins = append(plugins, kit.DescribeCommand(c))
		}
	}

	// Skills refused in v1 (script-bearing) are not registered as tools,
	// so surface them here as KindTool entries with an explicit refusal
	// note — a silent skip would read as "no such skill".
	for _, name := range skippedSkills {
		plugins = append(plugins, kit.Plugin{
			Kind:        kit.KindTool,
			Name:        skill.ToolNamePrefix + name,
			Description: "skill refused — requests shell execution (needs bash approval, not yet available)",
			Source:      ".project/skills/" + name,
		})
	}

	return kit.RenderPlugins(plugins)
}
