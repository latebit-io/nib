package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/llmconfig"
	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/extension"
	"github.com/latebit-io/nib/kit/extension/workspace"
	"github.com/latebit-io/nib/kit/mcp"
	"github.com/latebit-io/nib/kit/memory"
	"github.com/latebit-io/nib/kit/nib"
)

// choices is what the person chose for a session, from configuration
// and flags.
type choices struct {
	switches extension.Switches
	approve  bool
	limits   nib.Limits
	session  string // the session's id, for hooks
	// providerFor builds a subagent's model on the profile in use.
	providerFor func(model, effort string) (llm.Provider, error)
}

// providerFor builds providers on the profile current returns, with a
// subagent's model and effort in place of the session's. Each is built
// once and kept: a provider holds its connections and its output limit.
func providerFor(current func() *llmconfig.Resolved) func(model, effort string) (llm.Provider, error) {
	var mu sync.Mutex
	built := map[string]llm.Provider{}
	return func(model, effort string) (llm.Provider, error) {
		next := *current()
		next.Model = cmp.Or(model, next.Model)
		next.Effort = cmp.Or(effort, next.Effort)
		key := next.Profile + "\x00" + next.Model + "\x00" + next.Effort
		mu.Lock()
		defer mu.Unlock()
		if p, ok := built[key]; ok {
			return p, nil
		}
		p := next.NewProvider()
		if p == nil {
			return nil, fmt.Errorf("no provider for model %q on profile %s", next.Model, next.Profile)
		}
		built[key] = p
		return p, nil
	}
}

// assemble builds the session's blocks for root: the one assembly a
// run, a session and `--plugins` all use, so the manifest cannot drift
// from the run. Warnings go to stderr. The caller closes the result.
func assemble(ctx context.Context, root, prompt string, store memory.Store, chosen choices) (*nib.Assembly, error) {
	registry, err := extension.NewRegistry(workspace.Spec())
	if err != nil {
		return nil, err
	}
	// Without the store, servers that need a sign-in fail with a reason.
	tokens, err := tokenStore()
	if err != nil {
		warn("MCP sign-in store", err)
	}
	// Without the store, no plugin loads: the session still works.
	plugins, err := pluginStore()
	if err != nil {
		warn("plugin store", err)
	}
	a, err := nib.Assemble(ctx, nib.Options{
		Dir:          root,
		SystemPrompt: prompt,
		Store:        store,
		MCP:          mcpDiscovery(root),
		MCPTokens:    tokens,
		A2A:          a2aAgents(),
		Extensions:   registry,
		Switches:     chosen.switches,
		Limits:       chosen.limits,
		Plugins:      plugins,
		SessionID:    chosen.session,
		ProviderFor:  chosen.providerFor,
	})
	if err != nil {
		return nil, err
	}
	printWarnings(a.Warnings)
	return a, nil
}

// printWarnings reports what an assembly survived. Servers waiting
// for approval get the command that gives it.
func printWarnings(warnings []error) {
	for _, w := range warnings {
		if errors.Is(w, mcp.ErrUntrusted) {
			fmt.Fprintf(os.Stderr, "note: %v; run nib --trust-mcp to allow them\n", w)
			continue
		}
		if login, ok := errors.AsType[*mcp.LoginRequiredError](w); ok {
			fmt.Fprintf(os.Stderr, "note: %v; run nib --mcp-login %s\n", w, login.Server)
			continue
		}
		fmt.Fprintf(os.Stderr, "warning: %v\n", w)
		slog.Warn("assemble", "err", w)
	}
}

// configuredSwitches reads the person's extension choices. A file that
// cannot be used is reported and ignored: the defaults still give a
// working session.
func configuredSwitches() extension.Switches {
	path, err := extension.DefaultSwitchesPath()
	if err != nil {
		warn("extension config", err)
		return extension.Switches{}
	}
	switches, err := extension.LoadSwitches(path)
	if err != nil {
		warn("extension config", err)
	}
	return switches
}

// closeAssembly stops what the assembly started (MCP servers).
func closeAssembly(a *nib.Assembly) {
	if err := a.Close(); err != nil {
		slog.Warn("assembly close", "err", err)
	}
}

// printPlugins prints the manifest: the provider, the store, and every
// block the session would run with. A broken sign-in store degrades to
// "(none)": the manifest is a diagnostic and must still print.
func printPlugins(ctx context.Context, root string, store memory.Store, chosen choices) error {
	resolved, err := nib.ResolveProvider(root)
	if err != nil {
		slog.Debug("plugins: oauth store unavailable", "err", err)
	}
	provider := kit.Plugin{Kind: kit.KindProvider, Name: "(none)", Description: "no LLM credentials — " + credentialHint(resolved)}
	if resolved.HasProvider() {
		provider = kit.Plugin{Kind: kit.KindProvider, Name: resolved.Profile, Description: resolved.DisplayModel()}
	}
	a, err := assemble(ctx, root, "", store, chosen)
	if err != nil {
		return setupErr("%v", err)
	}
	defer closeAssembly(a)

	plugins := append([]kit.Plugin{
		provider,
		{Kind: kit.KindStore, Name: "demarkus", Description: "Mark Protocol versioned memory"},
	}, a.Plugins...)
	fmt.Print(kit.RenderPlugins(plugins))
	return nil
}
