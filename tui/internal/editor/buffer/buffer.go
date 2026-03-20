// Package buffer provides a line-array text buffer with undo/redo support.
package buffer

import (
	"os"
	"strings"
)

// Buffer is a line-array text buffer with undo/redo.
type Buffer struct {
	Lines    [][]rune
	Path     string
	Modified bool

	undo []operation
	redo []operation

	// grouping: operations between BeginGroup/EndGroup are undone/redone together
	grouping bool
	groupOps []operation
}

type opKind int

const (
	opInsert opKind = iota
	opDelete
	opGroupStart
	opGroupEnd
)

type operation struct {
	Kind opKind
	Line int
	Col  int
	Text []rune // for insert: what was inserted. for delete: what was deleted.
}

// New creates an empty buffer.
func New() *Buffer {
	return &Buffer{
		Lines: [][]rune{{}},
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
	b.Lines = make([][]rune, len(raw))
	for i, l := range raw {
		b.Lines[i] = []rune(l)
	}
	if len(b.Lines) == 0 {
		b.Lines = [][]rune{{}}
	}
	b.Modified = false
}

// LineCount returns the number of lines.
func (b *Buffer) LineCount() int {
	return len(b.Lines)
}

// LineLen returns the length of line at index line.
func (b *Buffer) LineLen(line int) int {
	if line < 0 || line >= len(b.Lines) {
		return 0
	}
	return len(b.Lines[line])
}

// LineText returns the string content of a line.
func (b *Buffer) LineText(line int) string {
	if line < 0 || line >= len(b.Lines) {
		return ""
	}
	return string(b.Lines[line])
}

// Content returns the full buffer content as a string.
func (b *Buffer) Content() string {
	parts := make([]string, len(b.Lines))
	for i, l := range b.Lines {
		parts[i] = string(l)
	}
	return strings.Join(parts, "\n")
}

// Save writes the buffer to its file path.
func (b *Buffer) Save() error {
	if b.Path == "" {
		return nil
	}
	content := b.Content() + "\n"
	err := os.WriteFile(b.Path, []byte(content), 0644)
	if err == nil {
		b.Modified = false
	}
	return err
}

// Insert inserts text at the given position. Handles newlines.
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

	// Record for undo
	b.pushUndo(operation{Kind: opDelete, Line: line, Col: col, Text: deleted})
	b.redo = nil

	b.doDelete(line, col, deleted)
	b.Modified = true
	return string(deleted)
}

// Undo undoes the last operation or group.
func (b *Buffer) Undo() (int, int, bool) {
	if len(b.undo) == 0 {
		return 0, 0, false
	}

	op := b.undo[len(b.undo)-1]
	b.undo = b.undo[:len(b.undo)-1]

	// Group end = undo ops in reverse order, push to redo in forward order
	if op.Kind == opGroupEnd {
		var lastLine, lastCol int
		var ops []operation
		for len(b.undo) > 0 {
			op = b.undo[len(b.undo)-1]
			b.undo = b.undo[:len(b.undo)-1]
			if op.Kind == opGroupStart {
				break
			}
			lastLine, lastCol = b.applyUndoOp(op)
			ops = append(ops, op)
		}
		// ops were undone in reverse (LIFO). Push to redo so that
		// redo stack top = GroupEnd, then ops in original order, then GroupStart.
		// redo pops GroupEnd first, collects ops, replays forward.
		b.redo = append(b.redo, operation{Kind: opGroupStart})
		// ops[0] was the last undone (originally first inserted) — push in collected order
		b.redo = append(b.redo, ops...)
		b.redo = append(b.redo, operation{Kind: opGroupEnd})
		return lastLine, lastCol, true
	}

	l, c := b.applyUndoOp(op)
	b.redo = append(b.redo, op)
	return l, c, true
}

// Redo redoes the last undone operation or group.
func (b *Buffer) Redo() (int, int, bool) {
	if len(b.redo) == 0 {
		return 0, 0, false
	}

	op := b.redo[len(b.redo)-1]
	b.redo = b.redo[:len(b.redo)-1]

	// Group end = collect ops (popped in forward order), replay sequentially
	if op.Kind == opGroupEnd {
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
			lastLine, lastCol = b.applyRedoOp(o)
			b.undo = append(b.undo, o)
		}
		b.undo = append(b.undo, operation{Kind: opGroupEnd})
		return lastLine, lastCol, true
	}

	l, c := b.applyRedoOp(op)
	b.undo = append(b.undo, op)
	return l, c, true
}

// BeginGroup starts a group of operations that will be undone/redone together.
func (b *Buffer) BeginGroup() {
	b.grouping = true
	b.groupOps = nil
}

// EndGroup ends a group of operations.
func (b *Buffer) EndGroup() {
	if !b.grouping {
		return
	}
	b.grouping = false
	if len(b.groupOps) == 0 {
		return
	}
	// Push group markers + ops onto undo stack
	b.undo = append(b.undo, operation{Kind: opGroupStart})
	b.undo = append(b.undo, b.groupOps...)
	b.undo = append(b.undo, operation{Kind: opGroupEnd})
	b.groupOps = nil
	b.redo = nil
}

// --- internal ---

func (b *Buffer) clamp(line, col int) (int, int) {
	if line < 0 {
		line = 0
	}
	if line >= len(b.Lines) {
		line = len(b.Lines) - 1
	}
	if col < 0 {
		col = 0
	}
	if col > len(b.Lines[line]) {
		col = len(b.Lines[line])
	}
	return line, col
}

func (b *Buffer) pushUndo(op operation) {
	if b.grouping {
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
		// Single-line insert
		b.Lines[line] = insertRunes(b.Lines[line], col, runes)
		return
	}

	// Multi-line insert
	after := append([]rune{}, b.Lines[line][col:]...)
	b.Lines[line] = append(b.Lines[line][:col], []rune(parts[0])...)

	// Insert middle lines
	newLines := make([][]rune, len(parts)-1)
	for i := 1; i < len(parts)-1; i++ {
		newLines[i-1] = []rune(parts[i])
	}
	// Last part + remainder of original line
	newLines[len(newLines)-1] = append([]rune(parts[len(parts)-1]), after...)

	// Splice into Lines array
	b.Lines = spliceLines(b.Lines, line+1, 0, newLines)
}

func (b *Buffer) doDelete(line, col int, runes []rune) {
	// Walk through runes and delete them
	curLine, curCol := line, col
	for _, r := range runes {
		if r == '\n' {
			// Join current line with next line
			if curLine+1 < len(b.Lines) {
				b.Lines[curLine] = append(b.Lines[curLine], b.Lines[curLine+1]...)
				b.Lines = spliceLines(b.Lines, curLine+1, 1, nil)
			}
		} else {
			if curLine < len(b.Lines) && curCol < len(b.Lines[curLine]) {
				b.Lines[curLine] = deleteRune(b.Lines[curLine], curCol)
			}
		}
	}
}

// applyUndoOp reverses a single operation. Does NOT push to redo stack.
func (b *Buffer) applyUndoOp(op operation) (int, int) {
	switch op.Kind {
	case opInsert:
		b.doDelete(op.Line, op.Col, op.Text)
		return op.Line, op.Col
	case opDelete:
		b.doInsert(op.Line, op.Col, op.Text)
		endLine, endCol := b.endOfInsert(op.Line, op.Col, op.Text)
		return endLine, endCol
	}
	return 0, 0
}

// applyRedoOp replays a single operation. Does NOT push to undo stack.
func (b *Buffer) applyRedoOp(op operation) (int, int) {
	switch op.Kind {
	case opInsert:
		b.doInsert(op.Line, op.Col, op.Text)
		endLine, endCol := b.endOfInsert(op.Line, op.Col, op.Text)
		return endLine, endCol
	case opDelete:
		b.doDelete(op.Line, op.Col, op.Text)
		return op.Line, op.Col
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
		if l >= len(b.Lines) {
			break
		}
		if c < len(b.Lines[l]) {
			result = append(result, b.Lines[l][c])
			c++
		} else if l+1 < len(b.Lines) {
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

func spliceLines(lines [][]rune, index, deleteCount int, insert [][]rune) [][]rune {
	result := make([][]rune, len(lines)-deleteCount+len(insert))
	copy(result, lines[:index])
	copy(result[index:], insert)
	copy(result[index+len(insert):], lines[index+deleteCount:])
	return result
}
