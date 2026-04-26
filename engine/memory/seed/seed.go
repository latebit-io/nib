// Package seed contains the canonical Junto project-memory document
// templates installed when a memory store is empty. Lives in its own
// package so the composition root (engine/wire) carries wiring code
// only — what a Junto project memory looks like is domain content.
package seed

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/latebit-io/junto/engine/memory"
)

// indexTemplate is the seed body for /index.md. The seven canonical
// sub-document references mirror the prompt layer's expectations:
// [Summary], [Journal], [Architecture], [Debugging] are referenced by
// the system prompt; new categories must be added in lockstep there.
const indexTemplate = `# Project Memory

## Project
- Name: %s

## Documents
- [Summary](/summary.md) — compact project snapshot (auto-injected on session start)
- [Journal](/journal.md) — session notes and progress
- [Architecture](/architecture.md) — design decisions and rationale
- [Debugging](/debugging.md) — lessons from investigations
`

// DefaultTimeout bounds the per-RPC deadline used when seeding. Callers
// override via [Install] with a context that already carries a deadline.
const DefaultTimeout = 10 * time.Second

// Install seeds /index.md if absent. Idempotent: a present index is
// left untouched, and a benign Publish race (another session seeded
// between Fetch and Publish) is treated as success rather than failure.
//
// projectRoot's basename names the project in the seed body. Pass an
// absolute path so the rendered name is stable across working
// directories.
func Install(ctx context.Context, store memory.Store, projectRoot string) error {
	checkCtx, checkCancel := context.WithTimeout(ctx, DefaultTimeout)
	_, err := store.Fetch(checkCtx, "/index.md")
	checkCancel()
	if err == nil {
		return nil
	}
	if !errors.Is(err, memory.ErrNotFound) {
		return fmt.Errorf("seed: check index: %w", err)
	}

	body := fmt.Sprintf(indexTemplate, filepath.Base(projectRoot))

	pubCtx, pubCancel := context.WithTimeout(ctx, DefaultTimeout)
	defer pubCancel()
	if _, err := store.Publish(pubCtx, "/index.md", body, 0); err != nil {
		if errors.Is(err, memory.ErrConflict) {
			return nil
		}
		return fmt.Errorf("seed: publish index: %w", err)
	}
	return nil
}
