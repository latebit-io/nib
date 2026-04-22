package session

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// newSessionID returns a new session identifier with the shape
// "YYYY-MM-DD-<8hex>". Used to group capture events for one Junto process
// lifetime. The random suffix distinguishes multiple processes started on
// the same day; callers treat the value as opaque.
//
// If the OS CSPRNG read fails, the helper falls back to a
// nanosecond-resolution timestamp suffix. Unique enough for dev-time
// session grouping; not collision-free across machines.
func newSessionID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UTC().Format("2006-01-02-150405.000000000")
	}
	return time.Now().UTC().Format("2006-01-02") + "-" + hex.EncodeToString(b[:])
}
