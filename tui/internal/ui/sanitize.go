package ui

import (
	"strings"
	"unicode"
)

// sanitize strips ANSI escape sequences and non-printable control characters
// from text, preserving only printable runes, newlines, and tabs.
func sanitize(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))

	i := 0
	runes := []rune(s)
	for i < len(runes) {
		r := runes[i]

		// Strip ANSI escape sequences: ESC [ ... final byte
		if r == '\x1b' && i+1 < len(runes) && runes[i+1] == '[' {
			i += 2 // skip ESC [
			for i < len(runes) && runes[i] < 0x40 || runes[i] > 0x7e {
				if runes[i] >= 0x40 && runes[i] <= 0x7e {
					break
				}
				i++
			}
			if i < len(runes) {
				i++ // skip final byte
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
