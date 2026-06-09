package editor

import (
	"strings"
	"unicode/utf8"
)

// BeginUndoGroup starts a buffer undo group. Nested groups are supported.
// Use this to batch multiple edits (e.g. replace-all) into a single undo step.
func (e *Editor) BeginUndoGroup() { e.Buf.BeginGroup() }

// EndUndoGroup ends a buffer undo group started by BeginUndoGroup.
func (e *Editor) EndUndoGroup() { e.Buf.EndGroup() }

// ReplaceRange selects the range [line, col .. line, col+length) and replaces
// it with text. This is the atomic operation for find-and-replace — callers
// do not need to manipulate selection state directly.
func (e *Editor) ReplaceRange(line, col, length int, text string) {
	e.Buf.BeginGroup()
	defer e.Buf.EndGroup()
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

// maxFindMatches caps the number of matches FindAll returns.
// Prevents unbounded memory growth on large files with short queries.
const maxFindMatches = 10_000

// FindAll returns all matches for query in the buffer.
// When caseSensitive is false, strings.EqualFold is used for proper Unicode
// case folding. Returns nil if query is empty. Results are capped at
// maxFindMatches to prevent unbounded growth.
func (e *Editor) FindAll(query string, caseSensitive bool) []FindMatch {
	if query == "" {
		return nil
	}

	needleLen := utf8.RuneCountInString(query)
	queryRunes := []rune(query)

	var matches []FindMatch
	for line := 0; line < e.Buf.LineCount(); line++ {
		lineText := e.Buf.LineText(line)
		if caseSensitive {
			matches = findCaseSensitive(lineText, queryRunes, needleLen, line, matches)
		} else {
			matches = findCaseInsensitive(lineText, query, needleLen, line, matches)
		}
		if len(matches) >= maxFindMatches {
			break
		}
	}
	return matches
}

// findCaseSensitive appends matches by comparing rune slices directly (zero allocation).
func findCaseSensitive(lineText string, queryRunes []rune, needleLen, line int, matches []FindMatch) []FindMatch {
	runes := []rune(lineText)
	for col := 0; col <= len(runes)-needleLen; col++ {
		if runesEqual(runes[col:col+needleLen], queryRunes) {
			matches = append(matches, FindMatch{Line: line, Col: col, Len: needleLen})
			if len(matches) >= maxFindMatches {
				return matches
			}
		}
	}
	return matches
}

// findCaseInsensitive appends matches using strings.EqualFold on byte-offset
// substrings (string slicing shares backing array, no allocation per candidate).
func findCaseInsensitive(lineText, query string, needleLen, line int, matches []FindMatch) []FindMatch {
	// Build a rune-to-byte offset map for this line.
	byteOff := 0
	runeOffsets := make([]int, 0, len(lineText))
	for byteOff < len(lineText) {
		runeOffsets = append(runeOffsets, byteOff)
		_, size := utf8.DecodeRuneInString(lineText[byteOff:])
		byteOff += size
	}
	runeOffsets = append(runeOffsets, byteOff) // sentinel for end-of-string

	runeCount := len(runeOffsets) - 1
	for col := 0; col <= runeCount-needleLen; col++ {
		startByte := runeOffsets[col]
		endByte := runeOffsets[col+needleLen]
		// EqualFold may match candidates of different byte lengths (e.g. ß vs SS),
		// but we compare fixed rune windows so byte length can differ from needleBytes.
		// Use the actual byte span of the rune window.
		if strings.EqualFold(lineText[startByte:endByte], query) {
			matches = append(matches, FindMatch{Line: line, Col: col, Len: needleLen})
			if len(matches) >= maxFindMatches {
				return matches
			}
		}
	}
	return matches
}

// runesEqual compares two rune slices for equality without allocation.
func runesEqual(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
