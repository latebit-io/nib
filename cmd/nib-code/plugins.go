package main

import (
	"errors"
	"log/slog"
	"os"
	"strings"

	"github.com/latebit-io/nib/ai/llmconfig"
	"github.com/latebit-io/nib/coding/agent"
	"github.com/latebit-io/nib/coding/pluginhooks"
	"github.com/latebit-io/nib/kit"
	kitcmd "github.com/latebit-io/nib/kit/command"
	"github.com/latebit-io/nib/kit/hookrun"
	"github.com/latebit-io/nib/kit/hookspec"
	"github.com/latebit-io/nib/kit/memory"
	"github.com/latebit-io/nib/kit/pluginstore"
	"github.com/latebit-io/nib/kit/skill"
)

// buildHookDispatcher constructs the plugin lifecycle-hooks dispatcher
// from the enabled AND trusted plugins' converted hooks configs.
//
// The trust gate lives here: [pluginstore.Store.ActivePlugins] returns
// enabled plugins only, so each is additionally checked against the
// user's per-plugin trust grant before its hooks may run — hooks shell
// out, exactly like script-bearing skills, so they ride the same
// one-time-trust surface (`/plugin trust`).
//
// plugins arrives sorted by id, so the resulting config slice is ordered
// — making the dispatcher's "first deny wins" deterministic. A plugin
// with no hooks.json is skipped silently; a malformed one is logged and
// skipped so one bad plugin cannot disable hooks for the rest. Returns
// nil when no trusted plugin contributes hooks, so the agent holds no
// dispatcher and pays nothing per run.
func buildHookDispatcher(plugins []pluginstore.ActivePlugin, trusted func(string) bool, cwd string) *pluginhooks.Dispatcher {
	var configs []hookspec.Config
	for _, p := range plugins {
		if !trusted(p.ID) {
			continue
		}
		data, err := os.ReadFile(p.HooksConfigPath)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				slog.Warn("hooks: cannot read plugin hooks config", "plugin", p.ID, "err", err)
			}
			continue
		}
		cfg, err := hookspec.Parse(data)
		if err != nil {
			slog.Warn("hooks: skipping malformed plugin hooks config", "plugin", p.ID, "err", err)
			continue
		}
		configs = append(configs, cfg)
	}
	if len(configs) == 0 {
		return nil
	}
	return pluginhooks.New(configs, hookrun.Runner{}, cwd)
}

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
	skills skill.Result,
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
			// Skill tools are rendered explicitly below (with source and
			// shadow/refusal info) — skip them in the generic tool loop
			// to avoid listing them twice.
			if strings.HasPrefix(t.Definition().Function.Name, skill.ToolNamePrefix) {
				continue
			}
			plugins = append(plugins, kit.DescribeTool(t))
		}
	}

	if registry != nil {
		for _, c := range registry.List() {
			plugins = append(plugins, kit.DescribeCommand(c))
		}
	}

	plugins = append(plugins, skill.Plugins(skills)...)

	return kit.RenderPlugins(plugins)
}
