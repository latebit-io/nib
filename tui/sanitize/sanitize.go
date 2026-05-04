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
	inEscape   bool // true when we've seen ESC [ but not yet the final byte
	pendingESC bool // true when chunk ended with ESC (next chunk may start with [)
}

// Sanitize strips ANSI escapes and control chars from a text chunk.
// Safe to call repeatedly on successive chunks — tracks state across calls.
func (z *Sanitizer) Sanitize(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))

	runes := []rune(s)
	i := 0

	// Handle pending ESC from previous chunk
	if z.pendingESC {
		z.pendingESC = false
		if len(runes) > 0 && runes[0] == '[' {
			// ESC [ split across chunks — enter escape mode
			z.inEscape = true
			i = 1
		}
		// If first rune isn't [, the ESC was a lone control char (already stripped)
	}

	for i < len(runes) {
		r := runes[i]

		// If we're inside an incomplete escape sequence from a previous chunk,
		// skip bytes until we find the final byte (0x40-0x7e).
		if z.inEscape {
			if r >= 0x40 && r <= 0x7e {
				z.inEscape = false
			}
			i++
			continue
		}

		// Strip ANSI escape sequences: ESC [ ... final byte
		if r == '\x1b' {
			if i+1 < len(runes) && runes[i+1] == '[' {
				i += 2 // skip ESC [
				z.inEscape = true
				// Consume as much of the sequence as we can in this chunk
				for i < len(runes) {
					if runes[i] >= 0x40 && runes[i] <= 0x7e {
						z.inEscape = false
						i++ // skip final byte
						break
					}
					i++
				}
			} else if i+1 >= len(runes) {
				// ESC at end of chunk — set pending for next chunk
				z.pendingESC = true
				i++
			} else {
				// ESC followed by something other than [ — strip the ESC
				i++
			}
			continue
		}

		// Allow newlines and tabs
		if r == '\n' || r == '\t' {
			sb.WriteRune(r)
			i++
			continue
		}

		// Strip other control characters
		if unicode.IsControl(r) {
			i++
			continue
		}

		sb.WriteRune(r)
		i++
	}

	return sb.String()
}
