package editor

// IncrementalEdit manages a character-by-character edit on a buffer.
// It encapsulates the undo group lifecycle and position tracking so that
// frontends only need to call InsertChar/Complete/Abort — they cannot
// forget EndGroup or corrupt the undo stack.
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

	groupOpen bool
	done      bool
}

// BeginIncrementalEdit starts an incremental edit at (line, col).
// It opens an undo group and deletes searchRunes runes at that position.
// The caller then inserts replacement text char-by-char via InsertChar.
func (e *Editor) BeginIncrementalEdit(line, col, searchRunes int) *IncrementalEdit {
	e.Buf.BeginGroup()
	e.Buf.Delete(line, col, searchRunes)
	e.MarkDirty()

	return &IncrementalEdit{
		editor:    e,
		line:      line,
		col:       col,
		startLine: line,
		startCol:  col,
		groupOpen: true,
	}
}

// InsertChar inserts a single rune at the current position and advances
// the cursor. Returns the inserted rune's position before advancement.
func (ie *IncrementalEdit) InsertChar(r rune) {
	if ie.done {
		return
	}
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

// FinishLine inserts all runes from the provided slice up to (but not
// including) the next newline. Returns the number of runes consumed.
// Used for "clean boundary" yields — finish the current line before stopping.
func (ie *IncrementalEdit) FinishLine(runes []rune) int {
	if ie.done {
		return 0
	}
	consumed := 0
	for _, r := range runes {
		if r == '\n' {
			break
		}
		ie.editor.Buf.Insert(ie.line, ie.col, string(r))
		ie.col++
		consumed++
	}
	if consumed > 0 {
		ie.editor.MarkDirty()
	}
	return consumed
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
func (ie *IncrementalEdit) Complete() {
	if ie.done {
		return
	}
	if ie.groupOpen {
		ie.editor.Buf.EndGroup()
		ie.groupOpen = false
	}
	ie.done = true
}

// Abort closes the undo group without finishing. The partial edit remains
// in the buffer and is undoable as one unit via Ctrl+Z.
func (ie *IncrementalEdit) Abort() {
	if ie.done {
		return
	}
	if ie.groupOpen {
		ie.editor.Buf.EndGroup()
		ie.groupOpen = false
	}
	ie.done = true
}

// IsComplete returns true if Complete or Abort has been called.
func (ie *IncrementalEdit) IsComplete() bool {
	return ie.done
}
