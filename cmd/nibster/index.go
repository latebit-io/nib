package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/latebit-io/nib/kit/memory"
)

// indexWriteRetries bounds the optimistic-concurrency retry loop in
// [writeIndexEntry]. Two concurrent nibster runs can race on the
// index: both fetch version N, both attempt to publish/append at N+1,
// and the second loses with [memory.ErrConflict]. The retry re-fetches
// and re-attempts; if a third writer keeps winning the race we give up
// rather than spin indefinitely.
const indexWriteRetries = 5

// indexPath is the durable record of every session nibster has run.
// The binary owns writes to this path (not the agent) so the index
// stays consistent even when the agent runs out of budget mid-update.
const indexPath = "/nibster/index.md"

// sessionsDir is the directory under which per-session memory pages
// live. The agent writes to <sessionsDir>/<sessionID>.md via memory_publish.
const sessionsDir = "/nibster/sessions"

// indexHeader is the markdown prelude used when the index file is
// created on the very first session.
const indexHeader = "# nibster sessions\n\n"

// sessionStatus is the terminal state of a session run, recorded in the
// index entry. Plain text rather than emoji so the index is grep-friendly
// and stays inside the project's no-emoji convention.
type sessionStatus string

const (
	statusSuccess   sessionStatus = "ok"
	statusFailed    sessionStatus = "fail"
	statusCancelled sessionStatus = "cancelled"
)

// messageMaxLen caps the message text in an index entry, in runes (not
// bytes), so multi-byte international characters count once. Longer
// messages are truncated with an ellipsis so a verbose prompt doesn't
// blow up the width of the index file.
const messageMaxLen = 100

// writeIndexEntry appends one line to /nibster/index.md describing a
// completed session. Creates the index file if it does not yet exist.
//
// The expected_version=0 publish + ErrNotFound branch follows the
// memory.Store contract: 0 means "create new" and only succeeds when
// the path is fresh.
//
// Concurrent nibster runs can race on the index. The fetch+write
// sequence is wrapped in a bounded retry loop: on [memory.ErrConflict]
// (either the create-new contention or the append version mismatch)
// we re-fetch and re-attempt up to [indexWriteRetries] times. Any
// non-conflict error returns immediately so callers can distinguish
// "lost the race repeatedly" from "store is broken."
func writeIndexEntry(ctx context.Context, store memory.Store, sessionID, message string, status sessionStatus) error {
	line := formatEntry(sessionID, message, status)

	var lastConflict error
	for attempt := 0; attempt < indexWriteRetries; attempt++ {
		err := writeIndexEntryOnce(ctx, store, line)
		if err == nil {
			return nil
		}
		if !errors.Is(err, memory.ErrConflict) {
			return err
		}
		lastConflict = err
	}
	return fmt.Errorf("write index: gave up after %d conflicts: %w", indexWriteRetries, lastConflict)
}

// writeIndexEntryOnce performs a single fetch-then-write attempt
// against the index. Returns [memory.ErrConflict] when an optimistic
// version mismatch is detected; the caller retries.
func writeIndexEntryOnce(ctx context.Context, store memory.Store, line string) error {
	doc, err := store.Fetch(ctx, indexPath)
	if errors.Is(err, memory.ErrNotFound) {
		_, perr := store.Publish(ctx, indexPath, indexHeader+line, 0)
		return perr
	}
	if err != nil {
		return fmt.Errorf("fetch index: %w", err)
	}
	if _, err := store.Append(ctx, indexPath, line, doc.Version); err != nil {
		return fmt.Errorf("append index: %w", err)
	}
	return nil
}

// formatEntry produces the markdown line written to the index for a
// single session. Format:
//
//   - [<sessionID>](sessions/<sessionID>.md) — "<message>" — <status>\n
//
// The message is quoted (newlines collapsed) and truncated at
// [messageMaxLen]. Status is the plain-text marker.
func formatEntry(sessionID, message string, status sessionStatus) string {
	return fmt.Sprintf("- [%s](sessions/%s.md) — %s — %s\n",
		sessionID, sessionID, quoteMessage(message), string(status))
}

// quoteMessage returns a Go-style quoted, single-line, length-capped
// representation of a free-form prompt for inclusion in the index.
// CRLF, LF, and bare CR all collapse to a single space so identical
// logical input renders identically regardless of line-ending source.
//
// Truncation operates on runes, not bytes, so multi-byte characters in
// international text are never split mid-sequence. (A byte slice could
// land inside a rune; strconv.Quote would then emit \xNN escapes for
// the orphaned bytes — parseable, but garbled.)
func quoteMessage(m string) string {
	m = strings.ReplaceAll(m, "\r\n", " ")
	m = strings.ReplaceAll(m, "\n", " ")
	m = strings.ReplaceAll(m, "\r", " ")
	if runes := []rune(m); len(runes) > messageMaxLen {
		m = string(runes[:messageMaxLen-3]) + "..."
	}
	return strconv.Quote(m)
}
