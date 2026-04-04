package wire

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/latebit-io/junto/engine/memory"
	memserver "github.com/latebit-io/junto/engine/memory/server"
)

// maxSummaryBytes caps the summary fetched at startup. The prompt layer
// also caps at 8 KB before template rendering; this avoids carrying a
// large string through the entire stack.
const maxSummaryBytes = 8000

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
func StartMemory(projectRoot string) (*MemoryResult, error) {
	mgr := memserver.New(projectRoot)

	token, err := mgr.EnsureToken()
	if err != nil {
		return nil, fmt.Errorf("bootstrap token: %w", err)
	}

	if _, err := mgr.Start(); err != nil {
		return nil, fmt.Errorf("start server: %w", err)
	}

	store := mgr.NewStore(token)

	stopAndFail := func(reason string, err error) (*MemoryResult, error) {
		if stopErr := mgr.Stop(); stopErr != nil {
			slog.Warn("memory: stop failed during rollback", "stopErr", stopErr)
		}
		return nil, fmt.Errorf("%s: %w", reason, err)
	}

	if err := seedMemory(store, projectRoot); err != nil {
		return stopAndFail("seed", err)
	}

	var summary string
	doc, err := store.Fetch(context.Background(), "/summary.md")
	switch {
	case err == nil:
		summary = doc.Body
		if len(summary) > maxSummaryBytes {
			summary = summary[:maxSummaryBytes]
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

// seedMemory creates the initial index.md if the memory store is empty.
// The seed document provides a navigable hub linking to the four core
// memory documents that the agent prompt references (summary, journal,
// architecture, debugging). This ensures new projects have a working
// memory structure from the first session.
func seedMemory(store memory.Store, projectRoot string) error {
	_, err := store.Fetch(context.Background(), "/index.md")
	if err == nil {
		return nil // already seeded
	}
	if !errors.Is(err, memory.ErrNotFound) {
		return fmt.Errorf("check index: %w", err)
	}

	projectName := filepath.Base(projectRoot)
	seed := fmt.Sprintf(`# Project Memory

## Project
- Name: %s

## Documents
- [Summary](/summary.md) — compact project snapshot (auto-injected on session start)
- [Journal](/journal.md) — session notes and progress
- [Architecture](/architecture.md) — design decisions and rationale
- [Debugging](/debugging.md) — lessons from investigations
`, projectName)

	if _, err := store.Publish(context.Background(), "/index.md", seed, 0); err != nil {
		return fmt.Errorf("publish index: %w", err)
	}
	return nil
}
