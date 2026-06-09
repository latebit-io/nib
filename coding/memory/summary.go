package memory

import (
	"context"
	"log/slog"

	kitmemory "github.com/latebit-io/nib/kit/memory"
)

// FetchSummary fetches /summary.md from the memory store and returns its
// body, falling back to fallback on any error. The agent's run loop calls
// this before each goal so every conversation sees the latest state.
//
// A nil store short-circuits to fallback (typical when memory is not
// configured). Errors are logged at debug level — the common case is "no
// summary published yet," which is a fine state for a fresh project.
// fallback is the startup-time summary so the prompt always has a usable
// snapshot even when the store is unreachable mid-run.
func FetchSummary(ctx context.Context, store kitmemory.Store, fallback string) string {
	if store == nil {
		return fallback
	}
	doc, err := store.Fetch(ctx, "/summary.md")
	if err != nil {
		slog.Debug("memory: refresh summary failed, using startup value", "err", err)
		return fallback
	}
	return doc.Body
}
