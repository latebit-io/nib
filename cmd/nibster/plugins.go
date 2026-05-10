package main

import (
	"fmt"

	"github.com/latebit-io/nib/ai/llmconfig"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/memory"
	"github.com/latebit-io/nib/kit/tools/bash"
	memorytools "github.com/latebit-io/nib/kit/tools/memory"
)

// nibsterToolset returns the canonical nibster Toolset — the same
// bundle used by [runAgent] and by [printPlugins]. Sharing a single
// constructor keeps the `--plugins` output honest: any tool listed
// here is exactly the tool the agent would dispatch at runtime.
func nibsterToolset(root string, store memory.Store) kit.Toolset {
	return kit.Toolset{
		Tools: []kit.Tool{
			bash.New(root),
			memorytools.NewFetchTool(store),
			memorytools.NewPublishTool(store),
			memorytools.NewAppendTool(store),
			memorytools.NewListTool(store),
		},
	}
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
		if oauthStore, err := openOAuthStore(); err == nil {
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

	ts := nibsterToolset(root, store)
	for _, t := range ts.Tools {
		plugins = append(plugins, kit.DescribeTool(t))
	}

	fmt.Print(kit.RenderPlugins(plugins))
	return nil
}
