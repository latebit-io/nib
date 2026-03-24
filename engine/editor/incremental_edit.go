package editor

import "github.com/latebit-io/junto/engine/buffer"

// IncrementalEdit manages a character-by-character edit on a buffer.
// It encapsulates the undo group lifecycle, position tracking, and
// per-tick advancement so that frontends only need to call Advance on
// each animation frame — they cannot forget EndGroup or corrupt the
// undo stack.
//
// Create via Editor.BeginIncrementalEdit. The edit is a single undo group:
// Complete or Abort closes it. Undo reverses the entire edit atomically.
type IncrementalEdit struct {
	editor *Editor

	// Current insert position (buffer coordinates, 0-indexed).
	line int
	col  int

	// Start of the edited region — used for collision detection.
	startLine int
	startCol  int

	// Replacement text and progress.
	replaceRunes []rune
	typed        int
	charsPerTick int

	// lineOrigins holds per-line provenance for the replacement text.
	// Index 0 = startLine, index 1 = startLine+1, etc. Nil slice means
	// no change. A nil entry within the slice means "keep current origin."
	lineOrigins []*buffer.Origin

	groupOpen bool
	completed bool
}

// AdvanceResult describes what happened during an Advance call.
type AdvanceResult struct {
	// Done is true when all replacement characters have been inserted.
	Done bool
	// HitNewline is true if the advance stopped early at a newline boundary.
	HitNewline bool
}

// BeginIncrementalEdit starts an incremental edit at (line, col).
// It opens an undo group, deletes searchRunes runes, and prepares to insert
// the replacement text at charsPerTick characters per Advance call.
// lineOrigins is a per-line origin slice for the replacement text (index 0 =
// first replacement line). Nil slice means no origin changes on Complete.
// A nil entry within the slice means "keep current origin" (unchanged line).
func (e *Editor) BeginIncrementalEdit(line, col, searchRunes, charsPerTick int, replace string, lineOrigins []*buffer.Origin) *IncrementalEdit {
	e.Buf.BeginGroup()
	e.Buf.Delete(line, col, searchRunes)
	e.MarkDirty()

	if charsPerTick < 1 {
		charsPerTick = 1
	}

	return &IncrementalEdit{
		editor:       e,
		line:         line,
		col:          col,
		startLine:    line,
		startCol:     col,
		replaceRunes: []rune(replace),
		charsPerTick: charsPerTick,
		lineOrigins:  lineOrigins,
		groupOpen:    true,
	}
}

// Advance inserts up to charsPerTick characters from the replacement text.
// Stops early after a newline (natural pause point). Returns what happened.
func (ie *IncrementalEdit) Advance() AdvanceResult {
	if ie.completed || ie.Remaining() == 0 {
		return AdvanceResult{Done: true}
	}

	for i := range ie.charsPerTick {
		if ie.typed >= len(ie.replaceRunes) {
			return AdvanceResult{Done: true}
		}
		r := ie.replaceRunes[ie.typed]
		ie.typed++
		ie.insertChar(r)

		// Pause after newline — stop this tick early.
		if r == '\n' && i < ie.charsPerTick-1 {
			return AdvanceResult{HitNewline: true}
		}
	}

	return AdvanceResult{Done: ie.typed >= len(ie.replaceRunes)}
}

// FinishLine inserts characters from the remaining replacement text up to
// (but not including) the next newline. Used for "clean boundary" yields.
func (ie *IncrementalEdit) FinishLine() {
	if ie.completed {
		return
	}
	dirty := false
	for ie.typed < len(ie.replaceRunes) {
		r := ie.replaceRunes[ie.typed]
		if r == '\n' {
			break
		}
		ie.typed++
		ie.editor.Buf.Insert(ie.line, ie.col, string(r))
		ie.col++
		dirty = true
	}
	if dirty {
		ie.editor.MarkDirty()
	}
}

// Remaining returns the number of replacement runes not yet inserted.
func (ie *IncrementalEdit) Remaining() int {
	return len(ie.replaceRunes) - ie.typed
}

// insertChar inserts a single rune and advances the cursor.
func (ie *IncrementalEdit) insertChar(r rune) {
	if r == '\n' {
		ie.editor.Buf.Insert(ie.line, ie.col, "\n")
		ie.line++
		ie.col = 0
	} else {
		ie.editor.Buf.Insert(ie.line, ie.col, string(r))
		ie.col++
	}
	ie.editor.MarkDirty()
}

// Position returns the current insert position (agent cursor location).
func (ie *IncrementalEdit) Position() (line, col int) {
	return ie.line, ie.col
}

// StartPosition returns where the edit began (for collision region start).
func (ie *IncrementalEdit) StartPosition() (line, col int) {
	return ie.startLine, ie.startCol
}

// Complete closes the undo group. The edit is final and undoable as one unit.
// If lineOrigins was provided, each line in the edited range is marked with
// its corresponding origin before the group closes (atomic with text changes).
func (ie *IncrementalEdit) Complete() {
	if ie.completed {
		return
	}
	ie.applyLineOrigins()
	if ie.groupOpen {
		ie.editor.Buf.EndGroup()
		ie.groupOpen = false
	}
	ie.completed = true
}

// Abort closes the undo group without finishing. The partial edit remains
// in the buffer and is undoable as one unit via Ctrl+Z.
// Origins are still applied to lines that were touched before the abort.
func (ie *IncrementalEdit) Abort() {
	if ie.completed {
		return
	}
	ie.applyLineOrigins()
	if ie.groupOpen {
		ie.editor.Buf.EndGroup()
		ie.groupOpen = false
	}
	ie.completed = true
}

// applyLineOrigins sets per-line origins on the affected buffer range.
// Nil entries are skipped (unchanged lines keep their current origin).
func (ie *IncrementalEdit) applyLineOrigins() {
	if len(ie.lineOrigins) == 0 || !ie.groupOpen {
		return
	}
	for i, origin := range ie.lineOrigins {
		if origin == nil {
			continue
		}
		bufLine := ie.startLine + i
		if bufLine < ie.editor.Buf.LineCount() {
			ie.editor.Buf.SetLineOrigin(bufLine, *origin)
		}
	}
}

// IsComplete returns true if Complete or Abort has been called.
func (ie *IncrementalEdit) IsComplete() bool {
	return ie.completed
}
