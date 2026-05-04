// Package textarea provides a reusable multiline text editing component
// for Bubble Tea applications. It supports cursor navigation, selection,
// word boundaries, soft wrapping, and clipboard integration.
package textarea

import "github.com/mattn/go-runewidth"

// visualLine maps a single display row back to its source in the logical
// line array. The wrap cache is a flat slice of these — one per visual row.
type visualLine struct {
	logicalLine int    // index into TextArea.lines
	runeOffset  int    // starting rune within that logical line
	runes       []rune // the runes displayed on this visual row
}

// buildWrapCache rebuilds the visual line cache from logical lines.
// Called lazily when wrapDirty is true before any operation that needs
// visual coordinates (Render, CursorPosition, Up/Down movement).
func (t *TextArea) buildWrapCache() {
	if !t.wrapDirty {
		return
	}
	t.wrapDirty = false
	t.wrapCache = t.wrapCache[:0]

	w := t.width
	if w <= 0 {
		w = 80 // fallback
	}

	for li, line := range t.lines {
		if len(line) == 0 {
			t.wrapCache = append(t.wrapCache, visualLine{
				logicalLine: li,
				runeOffset:  0,
				runes:       nil,
			})
			continue
		}
		t.wrapLogicalLine(li, line, w)
	}
}

// wrapLogicalLine wraps a single logical line into visual lines and appends
// them to the wrap cache. Uses cell-width measurement for wide characters.
func (t *TextArea) wrapLogicalLine(li int, line []rune, width int) {
	start := 0
	for start < len(line) {
		cellW := 0
		fitEnd := start
		for i := start; i < len(line); i++ {
			rw := runewidth.RuneWidth(line[i])
			if cellW+rw > width {
				break
			}
			cellW += rw
			fitEnd = i + 1
		}

		// Everything remaining fits — last visual line.
		if fitEnd == len(line) {
			t.wrapCache = append(t.wrapCache, visualLine{
				logicalLine: li,
				runeOffset:  start,
				runes:       line[start:],
			})
			return
		}

		// Always make progress (wide char wider than display).
		if fitEnd == start {
			fitEnd = start + 1
		}

		// Try to break at a space in the second half.
		breakAt := fitEnd
		for i := fitEnd - 1; i > start+(fitEnd-start)/2; i-- {
			if line[i] == ' ' {
				breakAt = i + 1
				break
			}
		}

		t.wrapCache = append(t.wrapCache, visualLine{
			logicalLine: li,
			runeOffset:  start,
			runes:       line[start:breakAt],
		})
		start = breakAt
	}
}

// logicalToVisual converts a logical (line, col) to a visual (row, col).
// Returns (0, 0) if the cache is empty.
func (t *TextArea) logicalToVisual(logLine, logCol int) (int, int) {
	t.buildWrapCache()

	for vi, vl := range t.wrapCache {
		if vl.logicalLine != logLine {
			continue
		}
		endOffset := vl.runeOffset + len(vl.runes)
		// Cursor belongs to this visual line if it's within range,
		// or if it's at the end of the last visual line for this logical line.
		isLast := vi == len(t.wrapCache)-1 || t.wrapCache[vi+1].logicalLine != logLine
		if logCol >= vl.runeOffset && (logCol < endOffset || (logCol == endOffset && isLast)) {
			return vi, logCol - vl.runeOffset
		}
	}

	// Fallback: last visual line.
	if len(t.wrapCache) > 0 {
		last := t.wrapCache[len(t.wrapCache)-1]
		return len(t.wrapCache) - 1, len(last.runes)
	}
	return 0, 0
}

// visualToLogical converts a visual (row, col) to a logical (line, col).
// Clamps to valid ranges.
func (t *TextArea) visualToLogical(visRow, visCol int) (int, int) {
	t.buildWrapCache()

	if len(t.wrapCache) == 0 {
		return 0, 0
	}
	if visRow < 0 {
		visRow = 0
	}
	if visRow >= len(t.wrapCache) {
		visRow = len(t.wrapCache) - 1
	}

	vl := t.wrapCache[visRow]
	col := visCol
	if col < 0 {
		col = 0
	}
	if col > len(vl.runes) {
		col = len(vl.runes)
	}
	return vl.logicalLine, vl.runeOffset + col
}

// visualLineCount returns the total number of visual (wrapped) lines.
func (t *TextArea) visualLineCount() int {
	t.buildWrapCache()
	return len(t.wrapCache)
}
