package main

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/latebit-io/nib/ai/llmconfig"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/memory"
	"github.com/latebit-io/nib/kit/skill"
	"github.com/latebit-io/nib/kit/tools/bash"
	memorytools "github.com/latebit-io/nib/kit/tools/memory"
)

// nibsterToolset returns the canonical nibster Toolset — the same
// bundle used by [runAgent] and by [printPlugins]. Sharing a single
// constructor keeps the `--plugins` output honest: any tool listed
// here is exactly the tool the agent would dispatch at runtime.
//
// Skills are discovered the same way as in nib-code — via the shared
// kit/skill capability, NOT borrowed from the coding agent (which
// nibster, the kit-boundary smoke test, cannot import). [skill.Discover]
// resolves both the project-local and user-global layers (project
// shadows global). The returned Result carries loaded/refused/shadowed
// skills so [printPlugins] can surface each rather than dropping any
// silently.
func nibsterToolset(root string, store memory.Store) (kit.Toolset, skill.Result) {
	tools := []kit.Tool{
		bash.New(root),
		memorytools.NewFetchTool(store),
		memorytools.NewPublishTool(store),
		memorytools.NewAppendTool(store),
		memorytools.NewListTool(store),
	}

	skills, err := skill.Discover(root)
	if err != nil {
		slog.Warn("skills: some skills failed to load", "err", err)
	}
	tools = append(tools, skills.Tools...)

	return kit.Toolset{Tools: tools}, skills
}

// printPlugins prints the wired plug-in manifest for nibster. Lists
// the LLM provider (when configured), the demarkus store, and every
// tool the runtime would dispatch through [nibsterToolset]. nibster
// has no slash-command registry — the kit-boundary smoke test
// deliberately ships zero commands.
//
// Provider resolution mirrors [runAgent]: profile from local config,
// OAuth wiring if the profile is OAuth-backed, falling back to
// "(no LLM credentials)" when nothing resolves. Failure to wire
// OAuth surfaces as a manifest line, not a hard error — `--plugins`
// is diagnostic and must produce something useful even when the
// auth path is broken.
func printPlugins(root string, store memory.Store) error {
	var plugins []kit.Plugin

	_, resolved := llmconfig.Resolve(root)
	if resolved.OAuthProvider != "" {
		oauthStore, err := openOAuthStore()
		if err != nil {
			slog.Debug("plugins: oauth store unavailable", "err", err)
		} else {
			llmconfig.WireOAuth(resolved, oauthStore)
		}
	}
	if resolved.HasProvider() {
		plugins = append(plugins, kit.Plugin{
			Kind:        kit.KindProvider,
			Name:        resolved.Profile,
			Description: resolved.DisplayModel(),
		})
	} else {
		plugins = append(plugins, kit.Plugin{
			Kind:        kit.KindProvider,
			Name:        "(none)",
			Description: "no LLM credentials — " + credentialHint(resolved),
		})
	}

	plugins = append(plugins, kit.Plugin{
		Kind:        kit.KindStore,
		Name:        "demarkus",
		Description: "Mark Protocol versioned memory",
	})

	ts, skills := nibsterToolset(root, store)
	for _, t := range ts.Tools {
		// Skill tools are rendered explicitly below (with source and
		// shadow/refusal info); skip them in the generic loop so they
		// are not listed twice.
		if strings.HasPrefix(t.Definition().Function.Name, skill.ToolNamePrefix) {
			continue
		}
		plugins = append(plugins, kit.DescribeTool(t))
	}

	plugins = append(plugins, skill.Plugins(skills)...)

	fmt.Print(kit.RenderPlugins(plugins))
	return nil
}
