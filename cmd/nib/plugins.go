package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/latebit-io/nib/ai/brand"
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
}

// assemble builds the session's blocks for root: the one assembly a
// run, a session and `--plugins` all use, so the manifest cannot drift
// from the run. Warnings go to stderr. The caller closes the result.
func assemble(ctx context.Context, root, prompt string, store memory.Store, chosen choices) (*nib.Assembly, error) {
	registry, err := extension.NewRegistry(workspace.Spec())
	if err != nil {
		return nil, err
	}
	// Without the store, the project's servers stay refused: the safe side.
	trust, err := trustStore()
	if err != nil {
		warn("MCP trust store", err)
	}
	user, err := mcp.ParseSpec(os.Getenv(brand.EnvKeyMCP), brand.EnvKeyMCP)
	if err != nil {
		warn("MCP servers", err)
	}
	a, err := nib.Assemble(ctx, nib.Options{
		Dir:          root,
		SystemPrompt: prompt,
		Store:        store,
		MCP:          mcp.DiscoverOptions{User: user, Trust: trust},
		Extensions:   registry,
		Switches:     chosen.switches,
		Limits:       chosen.limits,
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
