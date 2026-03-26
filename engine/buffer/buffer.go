// Package buffer provides a line-array text buffer with undo/redo support.
package buffer

import (
	"errors"
	"os"
	"strings"
)

// Origin identifies who wrote a line.
type Origin uint8

const (
	OriginDeveloper Origin = iota // default — developer-written or loaded from disk
	OriginAgent                   // written by the agent (set after approval)
)

// Line is a single line in the buffer, carrying both text and provenance.
type Line struct {
	// Runes is the text content of the line.
	Runes []rune
	// Origin tracks who wrote this line (Developer or Agent).
	Origin Origin
}

// ContentChange represents an incremental edit to a document.
// Accumulated by Insert/Delete for consumers like LSP that need edit deltas.
type ContentChange struct {
	StartLine, StartCol int    // 0-indexed, rune-based
	EndLine, EndCol     int    // 0-indexed, rune-based (position before the edit)
	Text                string // replacement text
}

// Buffer is a line-array text buffer with undo/redo.
type Buffer struct {
	lines []Line
	// Path is the file path associated with this buffer (empty for unsaved).
	Path string
	// Modified is true when the buffer has unsaved changes.
	Modified bool

	undo []operation
	redo []operation

	// groupDepth tracks nested BeginGroup/EndGroup calls. Only the outermost
	// EndGroup commits the group to the undo stack and fires OnChange.
	groupDepth int
	groupOps   []operation

	// OnChange is called after a logical operation completes (Insert, Delete,
	// Undo, Redo, EndGroup). Suppressed during grouped/undo/redo sub-operations
	// so that one logical operation produces one callback. nil-safe.
	//
	// Threading contract: OnChange is invoked on the same goroutine that
	// performed the mutation. For TUI-driven edits this is the main goroutine;
	// for agent-driven edits via Workspace it may be the agent goroutine.
	// Implementations must not mutate shared UI state directly — marshal
	// updates to the main goroutine (e.g., by sending on a channel).
	OnChange func()

	// changes accumulates ContentChange entries from Insert/Delete/Undo/Redo.
	// Drained by DrainChanges — typically inside an OnChange callback.
	// Only accumulated when OnChange is non-nil to prevent unbounded growth.
	changes []ContentChange

	// suppressDepth > 0 suppresses OnChange callbacks (not change tracking).
	// Used by BeginGroup/EndGroup and Undo/Redo to batch sub-operations.
	suppressDepth int
}

type opKind int

const (
	opInsert opKind = iota
	opDelete
	opSetOrigin
	opGroupStart
	opGroupEnd
)

type operation struct {
	Kind opKind
	Line int
	Col  int
	Text []rune // for insert: what was inserted. for delete: what was deleted.

	// Origin fields — used only by opSetOrigin.
	NewOrigin  Origin
	PrevOrigin Origin

	// LineOrigins stores the origins of lines affected by opDelete.
	// Captured before deletion so undo can restore per-line provenance
	// when doInsert recreates split lines (which would otherwise inherit
	// the parent's current origin, losing the original per-line data).
	LineOrigins []Origin
}

// New creates an empty buffer.
func New() *Buffer {
	return &Buffer{
		lines: []Line{{Runes: []rune{}}},
	}
}

// NewFromString creates a buffer with the given content.
func NewFromString(s string) *Buffer {
	b := &Buffer{}
	b.loadString(s)
	return b
}

// NewFromFile reads a file into a buffer.
func NewFromFile(path string) (*Buffer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	b := &Buffer{}
	b.loadString(string(data))
	b.Path = path
	return b, nil
}

func (b *Buffer) loadString(s string) {
	// Remove trailing newline to avoid empty final line
	s = strings.TrimSuffix(s, "\n")
	raw := strings.Split(s, "\n")
	b.lines = make([]Line, len(raw))
	for i, l := range raw {
		b.lines[i] = Line{Runes: []rune(l)}
	}
	if len(b.lines) == 0 {
		b.lines = []Line{{Runes: []rune{}}}
	}
	b.Modified = false
}

// LineCount returns the number of lines.
func (b *Buffer) LineCount() int {
	return len(b.lines)
}

// LineLen returns the length of line at index line.
func (b *Buffer) LineLen(line int) int {
	if line < 0 || line >= len(b.lines) {
		return 0
	}
	return len(b.lines[line].Runes)
}

// LineText returns the string content of a line.
func (b *Buffer) LineText(line int) string {
	if line < 0 || line >= len(b.lines) {
		return ""
	}
	return string(b.lines[line].Runes)
}

// LineOrigin returns the origin of a line.
func (b *Buffer) LineOrigin(line int) Origin {
	if line < 0 || line >= len(b.lines) {
		return OriginDeveloper
	}
	return b.lines[line].Origin
}

// SetLineOrigin sets the origin of a single line. The change is tracked
// in the undo system so it can be reversed (and participates in groups).
func (b *Buffer) SetLineOrigin(line int, origin Origin) {
	if line < 0 || line >= len(b.lines) {
		return
	}
	prev := b.lines[line].Origin
	if prev == origin {
		return
	}
	b.pushUndo(operation{
		Kind:       opSetOrigin,
		Line:       line,
		NewOrigin:  origin,
		PrevOrigin: prev,
	})
	b.redo = nil
	b.lines[line].Origin = origin
}

// SetLineOrigins sets the origin of a range of lines starting at startLine.
// Each line change is tracked individually in the undo system.
func (b *Buffer) SetLineOrigins(startLine, count int, origin Origin) {
	for i := startLine; i < startLine+count; i++ {
		b.SetLineOrigin(i, origin)
	}
}

// ResetOriginToDeveloper flips a line's origin to Developer without tracking
// the change in the undo system. Used by developer editing paths where
// ownership transfer is one-way (developer always wins). This is intentionally
// not undoable — the developer touched the line, so it's theirs.
func (b *Buffer) ResetOriginToDeveloper(line int) {
	if line >= 0 && line < len(b.lines) {
		b.lines[line].Origin = OriginDeveloper
	}
}

// DrainChanges returns accumulated content changes and clears the log.
// Typically called inside an OnChange callback to get all changes from
// the logical operation that just completed.
func (b *Buffer) DrainChanges() []ContentChange {
	if len(b.changes) == 0 {
		return nil
	}
	out := b.changes
	b.changes = nil
	return out
}

// HasChanges reports whether there are undrained content changes.
func (b *Buffer) HasChanges() bool {
	return len(b.changes) > 0
}

// fireOnChange calls the OnChange callback if not suppressed.
func (b *Buffer) fireOnChange() {
	if b.suppressDepth == 0 && b.OnChange != nil {
		b.OnChange()
	}
}

// trackChange appends a ContentChange if change tracking is active
// (OnChange is non-nil). Skips when OnChange is nil to prevent
// unbounded growth of the changes slice.
func (b *Buffer) trackChange(startLine, startCol, endLine, endCol int, text string) {
	if b.OnChange == nil {
		return
	}
	b.changes = append(b.changes, ContentChange{
		StartLine: startLine, StartCol: startCol,
		EndLine: endLine, EndCol: endCol,
		Text: text,
	})
}

// Content returns the full buffer content as a string.
func (b *Buffer) Content() string {
	parts := make([]string, len(b.lines))
	for i, l := range b.lines {
		parts[i] = string(l.Runes)
	}
	return strings.Join(parts, "\n")
}

// Save writes the buffer to its file path.
// ErrNoPath is returned when Save is called on a buffer with no file path.
var ErrNoPath = errors.New("no file path")

// Save writes the buffer content to its file path.
func (b *Buffer) Save() error {
	if b.Path == "" {
		return ErrNoPath
	}
	content := b.Content() + "\n"
	err := os.WriteFile(b.Path, []byte(content), 0644)
	if err == nil {
		b.Modified = false
	}
	return err
}

// Insert inserts text at the given position. Handles newlines.
// INVARIANT: change is appended BEFORE fireOnChange so that DrainChanges
// inside the callback always sees the latest change.
func (b *Buffer) Insert(line, col int, text string) {
	runes := []rune(text)
	if len(runes) == 0 {
		return
	}

	line, col = b.clamp(line, col)

	// Record for undo
	b.pushUndo(operation{Kind: opInsert, Line: line, Col: col, Text: runes})
	b.redo = nil

	b.doInsert(line, col, runes)
	b.Modified = true

	// Track change for LSP consumers — insert is an empty range + new text.
	b.trackChange(line, col, line, col, text)
	b.fireOnChange()
}

// InsertWithOrigin inserts text and sets the origin of all affected lines.
// For multi-line inserts, the original line and all new lines receive the
// given origin. The insert and origin changes are grouped atomically so
// OnChange fires once after both the text and provenance are updated.
func (b *Buffer) InsertWithOrigin(line, col int, text string, origin Origin) {
	runes := []rune(text)
	if len(runes) == 0 {
		return
	}

	// Ensure atomicity: suppress OnChange from Insert until origins are set.
	// If the caller already has a group active, this is a no-op (the outer
	// group's suppressDepth already suppresses OnChange).
	ownGroup := b.groupDepth == 0
	if ownGroup {
		b.BeginGroup()
		defer b.EndGroup()
	}

	// Insert handles undo push, redo clear, doInsert, and Modified flag.
	line, col = b.clamp(line, col)
	b.Insert(line, col, text)

	// Mark affected lines with the given origin. SetLineOrigin pushes
	// its own undo ops (participates in the active group).
	endLine, _ := b.endOfInsert(line, col, runes)
	for i := line; i <= endLine && i < len(b.lines); i++ {
		b.SetLineOrigin(i, origin)
	}
}

// Delete deletes count runes starting at (line, col). Returns the deleted text.
func (b *Buffer) Delete(line, col, count int) string {
	if count <= 0 {
		return ""
	}

	line, col = b.clamp(line, col)

	// Collect the runes to delete
	deleted := b.collectRunes(line, col, count)
	if len(deleted) == 0 {
		return ""
	}

	// Capture origins of lines that will be merged/removed by this delete.
	// Needed so undo can restore per-line provenance after doInsert recreates them.
	var lineOrigins []Origin
	newlineCount := 0
	for _, r := range deleted {
		if r == '\n' {
			newlineCount++
		}
	}
	if newlineCount > 0 {
		lineOrigins = make([]Origin, newlineCount+1)
		for i := range lineOrigins {
			if line+i < len(b.lines) {
				lineOrigins[i] = b.lines[line+i].Origin
			}
		}
	}

	// Record for undo
	b.pushUndo(operation{Kind: opDelete, Line: line, Col: col, Text: deleted, LineOrigins: lineOrigins})
	b.redo = nil

	// Compute end position before deleting (for change tracking).
	endLine, endCol := b.endOfInsert(line, col, deleted)

	b.doDelete(line, col, deleted)
	b.Modified = true

	// Track change — delete is a range with empty replacement text.
	b.trackChange(line, col, endLine, endCol, "")
	b.fireOnChange()
	return string(deleted)
}

// Undo undoes the last operation or group.
// OnChange is suppressed during grouped undo and fires once at the end.
func (b *Buffer) Undo() (int, int, bool) {
	if len(b.undo) == 0 {
		return 0, 0, false
	}

	op := b.undo[len(b.undo)-1]
	b.undo = b.undo[:len(b.undo)-1]

	// Group end = undo ops in reverse order, push to redo in forward order
	if op.Kind == opGroupEnd {
		b.suppressDepth++
		defer func() {
			b.suppressDepth--
			b.fireOnChange()
		}()
		var lastLine, lastCol int
		var ops []operation
		for len(b.undo) > 0 {
			op = b.undo[len(b.undo)-1]
			b.undo = b.undo[:len(b.undo)-1]
			if op.Kind == opGroupStart {
				break
			}
			l, c := b.applyUndoOp(op)
			// Only update cursor from text ops — opSetOrigin returns (line, 0)
			// which would overwrite the real cursor position.
			if op.Kind != opSetOrigin {
				lastLine, lastCol = l, c
			}
			ops = append(ops, op)
		}
		// ops were undone in reverse (LIFO). Push to redo so that
		// redo stack top = GroupEnd, then ops in original order, then GroupStart.
		// redo pops GroupEnd first, collects ops, replays forward.
		b.redo = append(b.redo, operation{Kind: opGroupStart})
		// ops[0] was the last undone (originally first inserted) — push in collected order
		b.redo = append(b.redo, ops...)
		b.redo = append(b.redo, operation{Kind: opGroupEnd})
		b.Modified = true
		return lastLine, lastCol, true
	}

	l, c := b.applyUndoOp(op)
	b.redo = append(b.redo, op)
	b.Modified = true
	b.fireOnChange()
	return l, c, true
}

// Redo redoes the last undone operation or group.
// OnChange is suppressed during grouped redo and fires once at the end.
func (b *Buffer) Redo() (int, int, bool) {
	if len(b.redo) == 0 {
		return 0, 0, false
	}

	op := b.redo[len(b.redo)-1]
	b.redo = b.redo[:len(b.redo)-1]

	// Group end = collect ops (popped in forward order), replay sequentially
	if op.Kind == opGroupEnd {
		b.suppressDepth++
		defer func() {
			b.suppressDepth--
			b.fireOnChange()
		}()
		var ops []operation
		for len(b.redo) > 0 {
			op = b.redo[len(b.redo)-1]
			b.redo = b.redo[:len(b.redo)-1]
			if op.Kind == opGroupStart {
				break
			}
			ops = append(ops, op)
		}
		// Replay in collected order (which is original forward order)
		var lastLine, lastCol int
		b.undo = append(b.undo, operation{Kind: opGroupStart})
		for _, o := range ops {
			l, c := b.applyRedoOp(o)
			// Only update cursor from text ops — opSetOrigin returns (line, 0)
			// which would overwrite the real cursor position.
			if o.Kind != opSetOrigin {
				lastLine, lastCol = l, c
			}
			b.undo = append(b.undo, o)
		}
		b.undo = append(b.undo, operation{Kind: opGroupEnd})
		b.Modified = true
		return lastLine, lastCol, true
	}

	l, c := b.applyRedoOp(op)
	b.undo = append(b.undo, op)
	b.Modified = true
	b.fireOnChange()
	return l, c, true
}

// BeginGroup starts a group of operations that will be undone/redone together.
// OnChange is suppressed until the outermost EndGroup fires one callback.
// Nested calls increment the depth counter — all ops accumulate into the
// outermost group.
func (b *Buffer) BeginGroup() {
	if b.groupDepth == 0 {
		b.groupOps = nil // reset ops only for outermost group
	}
	b.groupDepth++
	b.suppressDepth++
}

// EndGroup ends a group of operations.
// Only the outermost EndGroup commits the group to the undo stack and fires OnChange.
func (b *Buffer) EndGroup() {
	if b.groupDepth == 0 {
		return
	}
	b.groupDepth--
	b.suppressDepth--
	if b.groupDepth > 0 {
		return // not the outermost group yet
	}
	if len(b.groupOps) == 0 {
		return
	}
	// Push group markers + ops onto undo stack
	b.undo = append(b.undo, operation{Kind: opGroupStart})
	b.undo = append(b.undo, b.groupOps...)
	b.undo = append(b.undo, operation{Kind: opGroupEnd})
	b.groupOps = nil
	b.redo = nil
	b.fireOnChange()
}

// --- internal ---

func (b *Buffer) clamp(line, col int) (int, int) {
	if line < 0 {
		line = 0
	}
	if line >= len(b.lines) {
		line = len(b.lines) - 1
	}
	if col < 0 {
		col = 0
	}
	if col > len(b.lines[line].Runes) {
		col = len(b.lines[line].Runes)
	}
	return line, col
}

func (b *Buffer) pushUndo(op operation) {
	if b.groupDepth > 0 {
		b.groupOps = append(b.groupOps, op)
	} else {
		b.undo = append(b.undo, op)
	}
}

func (b *Buffer) doInsert(line, col int, runes []rune) {
	// Split inserted text by newlines
	text := string(runes)
	parts := strings.Split(text, "\n")

	if len(parts) == 1 {
		// Single-line insert — line retains its origin
		b.lines[line].Runes = insertRunes(b.lines[line].Runes, col, runes)
		return
	}

	// Multi-line insert — new lines inherit parent origin
	parentOrigin := b.lines[line].Origin
	after := append([]rune{}, b.lines[line].Runes[col:]...)
	b.lines[line].Runes = append(b.lines[line].Runes[:col], []rune(parts[0])...)

	// Insert middle lines
	newLines := make([]Line, len(parts)-1)
	for i := 1; i < len(parts)-1; i++ {
		newLines[i-1] = Line{Runes: []rune(parts[i]), Origin: parentOrigin}
	}
	// Last part + remainder of original line
	newLines[len(newLines)-1] = Line{
		Runes:  append([]rune(parts[len(parts)-1]), after...),
		Origin: parentOrigin,
	}

	// Splice into lines array
	b.lines = spliceLines(b.lines, line+1, 0, newLines)
}

func (b *Buffer) doDelete(line, col int, runes []rune) {
	// Walk through runes and delete them
	curLine, curCol := line, col
	for _, r := range runes {
		if r == '\n' {
			// Join current line with next line — merged line retains first line's origin
			if curLine+1 < len(b.lines) {
				b.lines[curLine].Runes = append(b.lines[curLine].Runes, b.lines[curLine+1].Runes...)
				b.lines = spliceLines(b.lines, curLine+1, 1, nil)
			}
		} else {
			if curLine < len(b.lines) && curCol < len(b.lines[curLine].Runes) {
				b.lines[curLine].Runes = deleteRune(b.lines[curLine].Runes, curCol)
			}
		}
	}
}

// applyUndoOp reverses a single operation. Does NOT push to redo stack.
// Tracks content changes for LSP consumers via trackChange.
func (b *Buffer) applyUndoOp(op operation) (int, int) {
	switch op.Kind {
	case opInsert:
		// Undo an insert = delete the inserted text.
		endLine, endCol := b.endOfInsert(op.Line, op.Col, op.Text)
		b.doDelete(op.Line, op.Col, op.Text)
		b.trackChange(op.Line, op.Col, endLine, endCol, "")
		return op.Line, op.Col
	case opDelete:
		// Undo a delete = re-insert the deleted text.
		b.doInsert(op.Line, op.Col, op.Text)
		b.trackChange(op.Line, op.Col, op.Line, op.Col, string(op.Text))
		// Restore per-line origins that were captured before the delete.
		// doInsert makes new lines inherit the parent's origin, which loses
		// the original per-line provenance for cross-line deletes.
		for i, origin := range op.LineOrigins {
			lineIdx := op.Line + i
			if lineIdx >= 0 && lineIdx < len(b.lines) {
				b.lines[lineIdx].Origin = origin
			}
		}
		endLine, endCol := b.endOfInsert(op.Line, op.Col, op.Text)
		return endLine, endCol
	case opSetOrigin:
		if op.Line >= 0 && op.Line < len(b.lines) {
			b.lines[op.Line].Origin = op.PrevOrigin
		}
		return op.Line, 0
	}
	return 0, 0
}

// applyRedoOp replays a single operation. Does NOT push to undo stack.
// Tracks content changes for LSP consumers via trackChange.
func (b *Buffer) applyRedoOp(op operation) (int, int) {
	switch op.Kind {
	case opInsert:
		b.doInsert(op.Line, op.Col, op.Text)
		b.trackChange(op.Line, op.Col, op.Line, op.Col, string(op.Text))
		endLine, endCol := b.endOfInsert(op.Line, op.Col, op.Text)
		return endLine, endCol
	case opDelete:
		endLine, endCol := b.endOfInsert(op.Line, op.Col, op.Text)
		b.doDelete(op.Line, op.Col, op.Text)
		b.trackChange(op.Line, op.Col, endLine, endCol, "")
		return op.Line, op.Col
	case opSetOrigin:
		if op.Line >= 0 && op.Line < len(b.lines) {
			b.lines[op.Line].Origin = op.NewOrigin
		}
		return op.Line, 0
	}
	return 0, 0
}

func (b *Buffer) endOfInsert(line, col int, runes []rune) (int, int) {
	l, c := line, col
	for _, r := range runes {
		if r == '\n' {
			l++
			c = 0
		} else {
			c++
		}
	}
	return l, c
}

func (b *Buffer) collectRunes(line, col, count int) []rune {
	var result []rune
	l, c := line, col
	for i := 0; i < count; i++ {
		if l >= len(b.lines) {
			break
		}
		if c < len(b.lines[l].Runes) {
			result = append(result, b.lines[l].Runes[c])
			c++
		} else if l+1 < len(b.lines) {
			// At end of line — the "character" here is the newline
			result = append(result, '\n')
			l++
			c = 0
		} else {
			break
		}
	}
	return result
}

// --- rune slice helpers ---

func insertRunes(line []rune, col int, runes []rune) []rune {
	result := make([]rune, len(line)+len(runes))
	copy(result, line[:col])
	copy(result[col:], runes)
	copy(result[col+len(runes):], line[col:])
	return result
}

func deleteRune(line []rune, col int) []rune {
	result := make([]rune, len(line)-1)
	copy(result, line[:col])
	copy(result[col:], line[col+1:])
	return result
}

func spliceLines(lines []Line, index, deleteCount int, insert []Line) []Line {
	result := make([]Line, len(lines)-deleteCount+len(insert))
	copy(result, lines[:index])
	copy(result[index:], insert)
	copy(result[index+len(insert):], lines[index+deleteCount:])
	return result
}
