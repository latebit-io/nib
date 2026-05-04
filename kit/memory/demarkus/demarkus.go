// Package demarkus provides a high-level facade for connecting a kit-based
// agent to a demarkus memory server. It composes the binary install,
// auth-token bootstrap, server lifecycle, and MCP-store adapter into a
// single [Open] call so consumers do not need to know about the internal
// subpackages ([server], [mcpadapter], [seed]).
//
// The facade is the recommended entry point for kit consumers (research
// agents, ops agents, the coding agent's wire layer). Callers that need
// finer control (custom seeding, skipping the install step, etc.) can
// drop into the subpackages directly.
package demarkus

import (
	"context"
	"fmt"

	"github.com/latebit-io/nib/kit/mcp"
	"github.com/latebit-io/nib/kit/memory"
	"github.com/latebit-io/nib/kit/memory/demarkus/mcpadapter"
	"github.com/latebit-io/nib/kit/memory/demarkus/server"
)

// Result carries the outputs of a successful [Open] call. Callers must
// invoke [Result.Close] exactly once when finished — it stops the
// demarkus-server child process and closes the MCP client subprocess.
type Result struct {
	// Store is the connected memory store. Operations on it round-trip
	// through the demarkus-mcp client to the demarkus-server.
	Store memory.Store

	// Port is the TCP port the demarkus-server is listening on. Useful
	// for diagnostics, integration tests, and capture sinks.
	Port int

	close func() error
}

// Close stops the demarkus-server and the MCP client subprocess. Safe
// to call exactly once. Returns the underlying [server.Manager.Stop]
// error if the server fails to shut down cleanly.
func (r *Result) Close() error {
	if r == nil || r.close == nil {
		return nil
	}
	closeFn := r.close
	r.close = nil
	return closeFn()
}

// Open boots a demarkus-server for projectRoot and returns a connected
// memory store. It performs (in order): binary install, auth-token
// bootstrap, server start, and MCP-store-adapter creation. On any
// failure after the server has started, the server is stopped before
// returning.
//
// ctx bounds setup operations that may hit the network (currently none,
// but reserved). Server lifecycle itself is process-bounded; callers
// must invoke [Result.Close] to stop it.
func Open(_ context.Context, projectRoot string) (*Result, error) {
	mgr := server.New(projectRoot)

	if err := mgr.EnsureBinaries(); err != nil {
		return nil, fmt.Errorf("demarkus: install binaries: %w", err)
	}

	token, err := mgr.EnsureToken()
	if err != nil {
		return nil, fmt.Errorf("demarkus: bootstrap token: %w", err)
	}

	if _, err := mgr.Start(); err != nil {
		return nil, fmt.Errorf("demarkus: start server: %w", err)
	}

	store, err := mgr.NewStore(token, func(c *mcp.Client) memory.Store {
		return mcpadapter.New(c)
	})
	if err != nil {
		// Server started but client failed — roll back the server so
		// callers don't leak a process they never got a handle to.
		if stopErr := mgr.Stop(); stopErr != nil {
			return nil, fmt.Errorf("demarkus: new store: %w (rollback also failed: %v)", err, stopErr)
		}
		return nil, fmt.Errorf("demarkus: new store: %w", err)
	}

	return &Result{
		Store: store,
		Port:  mgr.Port(),
		close: mgr.Stop,
	}, nil
}
