package session

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"time"
)

// newSessionID returns a new session identifier with the shape
// "YYYY-MM-DD-<8hex>". Used to group capture events for one Junto process
// lifetime. The random suffix distinguishes multiple processes started on
// the same day; callers treat the value as opaque.
//
// If the OS CSPRNG read fails, the helper logs a warning and falls back
// to a nanosecond-resolution timestamp suffix. On modern Go this branch
// is effectively unreachable — the runtime treats OS CSPRNG failure as
// fatal — but the project's "never silently swallow errors" rule
// requires the branch be observable if it ever does fire.
func newSessionID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		slog.Warn("session: CSPRNG read failed; using timestamp fallback for session ID",
			"err", err)
		return time.Now().UTC().Format("2006-01-02-150405.000000000")
	}
	return time.Now().UTC().Format("2006-01-02") + "-" + hex.EncodeToString(b[:])
}
