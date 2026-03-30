package editor

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// ReplaceRange selects the range [line, col .. line, col+length) and replaces
// it with text. This is the atomic operation for find-and-replace — callers
// do not need to manipulate selection state directly.
func (e *Editor) ReplaceRange(line, col, length int, text string) {
	e.SelectStartLine = line
	e.SelectStartCol = col
	e.SelectionActive = true
	e.CursorLine = line
	e.CursorCol = col + length
	e.DeleteSelection()
	e.PasteText(text)
}

// FindMatch represents a single search match in the buffer.
type FindMatch struct {
	Line int // 0-based line index
	Col  int // 0-based rune column
	Len  int // match length in runes
}

// FindAll returns all matches for query in the buffer.
// When caseSensitive is false, both query and buffer text are lowercased
// before comparison. Returns nil if query is empty.
func (e *Editor) FindAll(query string, caseSensitive bool) []FindMatch {
	if query == "" {
		return nil
	}

	searchQuery := query
	if !caseSensitive {
		searchQuery = strings.ToLower(query)
	}
	needleLen := utf8.RuneCountInString(searchQuery)
	needleRunes := []rune(searchQuery)

	var matches []FindMatch
	for line := 0; line < e.Buf.LineCount(); line++ {
		lineText := e.Buf.LineText(line)
		var runes []rune
		if caseSensitive {
			runes = []rune(lineText)
		} else {
			runes = []rune(strings.ToLower(lineText))
		}
		for col := 0; col <= len(runes)-needleLen; col++ {
			if slices.Equal(runes[col:col+needleLen], needleRunes) {
				matches = append(matches, FindMatch{
					Line: line,
					Col:  col,
					Len:  needleLen,
				})
			}
		}
	}
	return matches
}
