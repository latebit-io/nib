package ui

import (
	"strings"
	"unicode"
)

// isLeakedMouseSequence detects SGR mouse escape sequence fragments that
// Bubble Tea's input parser failed to consume. These arrive as KeyRunes
// and look like: [<65;14;32M or <65;14;32M (with or without leading [).
func isLeakedMouseSequence(runes []rune) bool {
	if len(runes) < 4 {
		return false
	}
	s := runes
	// Skip optional leading [
	if s[0] == '[' {
		s = s[1:]
	}
	if len(s) < 3 || s[0] != '<' {
		return false
	}
	// Must end with M or m (SGR press/release)
	last := s[len(s)-1]
	if last != 'M' && last != 'm' {
		return false
	}
	// Middle must be digits and semicolons
	for _, r := range s[1 : len(s)-1] {
		if r != ';' && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// sanitizer strips ANSI escape sequences and non-printable control characters
// from streamed text. It is stateful to handle escape sequences split across
// chunks (e.g., ESC arrives in one TokenMsg, [ and params in the next).
type sanitizer struct {
	inEscape bool // true when we've seen ESC [ but not yet the final byte
}

func (z *sanitizer) sanitize(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))

	runes := []rune(s)
	i := 0
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
				// ESC at end of chunk — might be start of CSI split across chunks.
				// Drop it; if next chunk starts with [, inEscape handles it.
				// If not, the lone ESC is a control char we'd strip anyway.
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
