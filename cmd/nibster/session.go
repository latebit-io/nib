package main

import (
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

// newSessionID composes a session ID from a timestamp and the user's
// message. Format: <YYYY-MM-DD-HHMMSS>-<slug>.
func newSessionID(now time.Time, message string) string {
	return now.UTC().Format(sessionIDTimeFormat) + "-" + slugify(message)
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
