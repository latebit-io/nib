package main

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// sessionIDTimeFormat is the YYYY-MM-DD-HHMMSS prefix on every session
// ID. Sortable lexicographically, which is the format the index file's
// "newest first via append" relies on.
const sessionIDTimeFormat = "2006-01-02-150405"

// slugMaxLen caps the slug portion of a session ID. Long enough to read
// at a glance, short enough to keep the full ID under terminal width.
const slugMaxLen = 30

// entropyBytes is the size in bytes of the random suffix appended to
// every session ID. Two prompts started in the same second with the
// same slug would otherwise collide on /nibster/sessions/<id>.md and
// the index entry. 4 bytes = 8 hex chars = 2^32 collision space, more
// than enough for any plausible per-second nibster fan-out.
const entropyBytes = 4

// sessionIDPattern is the strict shape of a session ID. Used by
// [validSessionID] when parsing user-supplied IDs from --show so a
// path-like value cannot escape sessionsDir.
var sessionIDPattern = regexp.MustCompile(
	`^[0-9]{4}-[0-9]{2}-[0-9]{2}-[0-9]{6}-[a-z0-9-]+-[0-9a-f]{` + strconv.Itoa(entropyBytes*2) + `}$`,
)

// newSessionID composes a session ID from a timestamp, the user's
// message, and a random entropy suffix. Format:
// <YYYY-MM-DD-HHMMSS>-<slug>-<hex>. The hex tail prevents collisions
// when two prompts start in the same second with similar messages
// (concurrent or scripted runs).
func newSessionID(now time.Time, message string) string {
	return now.UTC().Format(sessionIDTimeFormat) + "-" + slugify(message) + "-" + entropy()
}

// entropy returns a hex-encoded random suffix of [entropyBytes] bytes.
// Reads from crypto/rand; on the (effectively impossible) read error
// it falls back to a deterministic zero string rather than panicking,
// since a session ID with weaker entropy still functions — it just
// regresses to the pre-suffix collision profile.
func entropy() string {
	var b [entropyBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strings.Repeat("0", entropyBytes*2)
	}
	return hex.EncodeToString(b[:])
}

// validSessionID reports whether id matches the canonical session-ID
// shape. Used by --show to reject path-like values (../foo, /etc, etc.)
// before they are concatenated into a store path.
func validSessionID(id string) bool {
	return sessionIDPattern.MatchString(id)
}

// slugify produces a filesystem-safe lowercase-alphanumeric slug from a
// free-form message. Non-alphanumeric runs collapse to a single hyphen;
// the slug is trimmed of leading/trailing hyphens and capped at
// [slugMaxLen]. An empty result falls back to "session" so every ID has
// a non-empty trailing segment.
func slugify(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(slugMaxLen)
	lastDash := false
	for _, r := range s {
		if b.Len() >= slugMaxLen {
			break
		}
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if out == "" {
		return "session"
	}
	return out
}
