package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/ai/oauth"
	"github.com/latebit-io/nib/kit/mcp"
	"github.com/latebit-io/nib/kit/nib"
)

// trustStore opens the store of approved project server lists.
func trustStore() (*mcp.TrustStore, error) {
	path, err := mcp.DefaultTrustPath()
	if err != nil {
		return nil, err
	}
	return mcp.OpenTrustStore(path), nil
}

// trustMCP records the project's .mcp.json as approved and prints the
// servers it allows, so the person sees what they agreed to.
func trustMCP(dir string) error {
	root := nib.ProjectRoot(dir)
	store, err := trustStore()
	if err != nil {
		return setupErr("%v", err)
	}
	cfg, err := store.Approve(root)
	if err != nil {
		return setupErr("%v", err)
	}
	fmt.Printf("Trusted %s in %s. A changed file must be trusted again.\n", mcp.ProjectConfigName, root)
	for _, s := range cfg.Servers {
		fmt.Printf("  %s: %s\n", s.Name, s.Target())
	}
	for _, r := range cfg.Refused {
		fmt.Printf("  %s: still refused: %v\n", r.Server.Name, r.Err)
	}
	return nil
}

// tokenStore opens the store of the person's sign-ins to HTTP servers.
func tokenStore() (*mcp.TokenStore, error) {
	path, err := mcp.DefaultTokenPath()
	if err != nil {
		return nil, err
	}
	return mcp.OpenTokenStore(path), nil
}

// mcpDiscovery is where a session's server definitions come from: the
// person's own list and the project's file, which needs approval.
func mcpDiscovery(root string) mcp.DiscoverOptions {
	// Without the store, the project's servers stay refused: the safe side.
	trust, err := trustStore()
	if err != nil {
		warn("MCP trust store", err)
	}
	user, err := mcp.ParseSpec(os.Getenv(brand.EnvKeyMCP), brand.EnvKeyMCP)
	if err != nil {
		warn("MCP servers", err)
	}
	return mcp.DiscoverOptions{ProjectDir: nib.ProjectRoot(root), User: user, Trust: trust}
}

// mcpLogin signs the person in to the named HTTP server through the
// browser and keeps the sign-in, which later sessions use and refresh.
func mcpLogin(root, name string) error {
	cfg, err := mcp.Discover(mcpDiscovery(root))
	if err != nil {
		warn("MCP config", err)
	}
	i := slices.IndexFunc(cfg.Servers, func(s mcp.Server) bool { return s.Name == name })
	if i < 0 {
		if j := slices.IndexFunc(cfg.Refused, func(r mcp.Refusal) bool { return r.Server.Name == name }); j >= 0 {
			return setupErr("MCP server %s is not started: %v", name, cfg.Refused[j].Err)
		}
		return setupErr("no MCP server is named %s", name)
	}
	store, err := tokenStore()
	if err != nil {
		return setupErr("%v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err = mcp.Login(ctx, cfg.Servers[i], store, mcp.LoginOptions{
		Open:   oauth.OpenBrowser,
		Notify: func(msg string) { fmt.Fprintln(os.Stderr, msg) },
	})
	switch {
	case errors.Is(err, mcp.ErrNoLoginNeeded):
		fmt.Printf("%s did not ask for a sign-in; nothing to keep.\n", name)
		return nil
	case err != nil:
		return setupErr("sign-in to %s: %v", name, err)
	}
	fmt.Printf("Signed in to %s. Sessions use this sign-in and refresh it.\n", name)
	return nil
}
