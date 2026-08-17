package ui

import (
	"strings"

	"github.com/latebit-io/nib/tui/sanitize"
)

// inlineWhitespace folds the line breaks Sanitizer lets through so a
// value renders on a single row (status bar, list entries).
var inlineWhitespace = strings.NewReplacer("\n", " ", "\t", " ")

// sanitizeInline strips ANSI escapes and control characters from external
// text and collapses newlines/tabs to spaces for single-row rendering.
func sanitizeInline(s string) string {
	var san sanitize.Sanitizer
	return inlineWhitespace.Replace(san.Sanitize(s))
}

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
