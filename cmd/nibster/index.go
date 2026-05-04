package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/latebit-io/nib/kit/memory"
)

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

// messageMaxLen caps the message text in an index entry. Longer messages
// are truncated with an ellipsis so a verbose prompt doesn't blow up the
// width of the index file.
const messageMaxLen = 100

// writeIndexEntry appends one line to /nibster/index.md describing a
// completed session. Creates the index file if it does not yet exist.
//
// The expected_version=0 publish + ErrNotFound branch follows the
// memory.Store contract: 0 means "create new" and only succeeds when
// the path is fresh.
func writeIndexEntry(ctx context.Context, store memory.Store, sessionID, message string, status sessionStatus) error {
	line := formatEntry(sessionID, message, status)

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
func quoteMessage(m string) string {
	m = strings.ReplaceAll(m, "\n", " ")
	m = strings.ReplaceAll(m, "\r", " ")
	if len(m) > messageMaxLen {
		m = m[:messageMaxLen-3] + "..."
	}
	return strconv.Quote(m)
}
