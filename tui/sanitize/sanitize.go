// Package sanitize strips ANSI escape sequences and non-printable control
// characters from streamed text. Any frontend displaying LLM output needs this.
package sanitize

import (
	"strings"
	"unicode"
)

// Sanitizer strips ANSI escape sequences and non-printable control characters
// from streamed text. It is stateful to handle escape sequences split across
// chunks (e.g., ESC arrives in one token, [ and params in the next).
type Sanitizer struct {
	inCSI      bool // inside ESC [ ... waiting for the final byte (0x40-0x7e)
	inOSC      bool // inside ESC ] ... waiting for BEL or ESC \
	pendingESC bool // last rune seen was ESC; the next rune decides the sequence kind
}

// Sanitize strips ANSI escapes and control chars from a text chunk.
// Safe to call repeatedly on successive chunks — tracks state across calls.
func (z *Sanitizer) Sanitize(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))

	for _, r := range s {
		if z.pendingESC {
			z.pendingESC = false
			switch {
			case r == '[':
				z.inCSI = true
				continue
			case r == ']':
				z.inOSC = true
				continue
			case r == '\\' && z.inOSC:
				z.inOSC = false // ST terminator
				continue
			}
			// Lone ESC: already dropped; r is handled normally below.
		}
		if z.inCSI {
			if r >= 0x40 && r <= 0x7e {
				z.inCSI = false
			}
			continue
		}
		if z.inOSC {
			switch r {
			case '\a':
				z.inOSC = false
			case '\x1b':
				z.pendingESC = true
			}
			continue
		}
		switch {
		case r == '\x1b':
			z.pendingESC = true
		case r == '\n' || r == '\t':
			sb.WriteRune(r)
		case unicode.IsControl(r):
			// dropped
		default:
			sb.WriteRune(r)
		}
	}

	return sb.String()
}
