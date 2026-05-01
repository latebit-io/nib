package wire

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/latebit-io/nib/engine/mcp"
	"github.com/latebit-io/nib/engine/memory"
	"github.com/latebit-io/nib/engine/memory/mcpadapter"
	"github.com/latebit-io/nib/engine/memory/seed"
	memserver "github.com/latebit-io/nib/engine/memory/server"
)

// maxSummaryBytes caps the summary fetched at startup. The prompt layer
// also caps at 8 KB before template rendering; this avoids carrying a
// large string through the entire stack.
const maxSummaryBytes = 8000

// memoryOpTimeout bounds individual memory RPCs during startup.
// Prevents indefinite blocking if the demarkus server stalls.
const memoryOpTimeout = 10 * time.Second

// EnsureBinaries installs demarkus binaries for the project if not present.
// Idempotent — skips if already installed. This should always be called,
// even without an agent, so the binaries are ready when needed.
func EnsureBinaries(projectRoot string) error {
	mgr := memserver.New(projectRoot)
	return mgr.EnsureBinaries()
}

// MemoryResult holds the outputs from StartMemory. Only returned on
// success — callers may defer Cleanup immediately. Cleanup is safe to
// call exactly once; it stops the demarkus server process.
type MemoryResult struct {
	// Store is the memory store connected to the running demarkus server.
	Store memory.Store
	// Summary is the project memory snapshot for the first prompt, or empty
	// if no summary exists yet.
	Summary string
	// Cleanup stops the demarkus server. Must be called exactly once.
	Cleanup func()
}

// StartMemory bootstraps auth, starts the demarkus server, seeds initial
// content, and returns the memory store, session summary, and cleanup
// function. Binaries must already be installed via EnsureBinaries.
//
// ctx bounds the seed and summary RPCs; on cancellation StartMemory
// stops the server it started so it doesn't leak. Pass a cancellable
// context when the caller can interrupt startup (e.g. SIGINT during
// boot); context.Background() is acceptable for non-interactive paths.
func StartMemory(ctx context.Context, projectRoot string) (*MemoryResult, error) {
	mgr := memserver.New(projectRoot)

	token, err := mgr.EnsureToken()
	if err != nil {
		return nil, fmt.Errorf("bootstrap token: %w", err)
	}

	if _, err := mgr.Start(); err != nil {
		return nil, fmt.Errorf("start server: %w", err)
	}

	stopAndFail := func(reason string, err error) (*MemoryResult, error) {
		if stopErr := mgr.Stop(); stopErr != nil {
			slog.Warn("memory: stop failed during rollback", "stopErr", stopErr)
		}
		return nil, fmt.Errorf("%s: %w", reason, err)
	}

	store, err := mgr.NewStore(token, func(c *mcp.Client) memory.Store { return mcpadapter.New(c) })
	if err != nil {
		return stopAndFail("new store", err)
	}

	if err := seed.Install(ctx, store, projectRoot); err != nil {
		return stopAndFail("seed", err)
	}

	var summary string
	fetchCtx, fetchCancel := context.WithTimeout(ctx, memoryOpTimeout)
	doc, err := store.Fetch(fetchCtx, "/summary.md")
	fetchCancel()
	switch {
	case err == nil:
		summary = doc.Body
		if len(summary) > maxSummaryBytes {
			// Truncate at a rune boundary to avoid splitting multi-byte UTF-8.
			cut := maxSummaryBytes
			for cut > 0 && !utf8.RuneStart(summary[cut]) {
				cut--
			}
			summary = summary[:cut]
		}
	case errors.Is(err, memory.ErrNotFound):
		// No summary yet — agent will create one.
	default:
		return stopAndFail("fetch summary", err)
	}

	cleanup := func() {
		if err := mgr.Stop(); err != nil {
			slog.Warn("memory: server stop failed", "err", err)
		}
	}

	slog.Info("memory: ready", "port", mgr.Port())
	return &MemoryResult{
		Store:   store,
		Summary: summary,
		Cleanup: cleanup,
	}, nil
}
