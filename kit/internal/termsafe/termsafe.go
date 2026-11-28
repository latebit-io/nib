// Package termsafe makes text from a server safe to print or to put in
// one line: control characters and direction overrides become spaces.
package termsafe

import (
	"strings"
	"unicode"
)

// Text replaces each control character (a newline, an escape sequence)
// and direction override with a space: either could forge a line.
func Text(text string) string {
	return strings.Map(func(c rune) rune {
		if unicode.IsControl(c) || unicode.Is(unicode.Bidi_Control, c) {
			return ' '
		}
		return c
	}, text)
}
