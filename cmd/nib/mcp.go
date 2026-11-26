package main

import (
	"fmt"
	"strings"

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
		fmt.Printf("  %s: %s\n", s.Name, strings.TrimSpace(s.Command+" "+strings.Join(s.Args, " ")))
	}
	for _, r := range cfg.Refused {
		fmt.Printf("  %s: still refused: %v\n", r.Server.Name, r.Err)
	}
	return nil
}
