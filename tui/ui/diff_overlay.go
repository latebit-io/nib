package ui

import (
	"strings"

	"github.com/latebit-io/nib/engine/buffer"
	"github.com/latebit-io/nib/engine/editor"
)

// DiffOverlay manages the inline diff preview and editable replacement lines.
// The replacement text lives in a real editor.Editor, so all editing operations
// (cursor, selection, text ops, undo/redo) come from the engine — no duplication.
type DiffOverlay struct {
	// StartLine is the first buffer line being replaced (0-indexed, inclusive).
	StartLine int
	// EndLine is the last buffer line being replaced (0-indexed, inclusive).
	EndLine int

	// Active is true when the cursor is in the replacement zone.
	Active bool

	// Editor wraps the replacement text. All cursor, selection, and text
	// operations are delegated here.
	Editor *editor.Editor
}

// NewDiffOverlay creates a DiffOverlay from a computed DiffResult.
// The replacement lines are loaded into a real buffer/editor.
func NewDiffOverlay(diff *editor.DiffResult) *DiffOverlay {
	content := strings.Join(diff.NewLines, "\n")
	buf := buffer.New()
	if content != "" {
		buf.Insert(0, 0, content)
	}
	e := editor.New(buf)
	// Set height large enough that EnsureCursorVisible works within the overlay
	// without interfering with the main viewport's scroll.
	e.SetSize(80, buf.LineCount()+10)

	return &DiffOverlay{
		StartLine: diff.StartLine,
		EndLine:   diff.EndLine,
		Editor:    e,
	}
}

// LineCount returns the number of replacement lines.
func (o *DiffOverlay) LineCount() int {
	return o.Editor.LineCount()
}

// LineText returns replacement line i as a string.
func (o *DiffOverlay) LineText(i int) string {
	if i < 0 || i >= o.Editor.LineCount() {
		return ""
	}
	return o.Editor.LineText(i)
}

// Content returns the full replacement text.
func (o *DiffOverlay) Content() string {
	return o.Editor.Content()
}

// MergedContent returns the full file content as if the overlay were applied.
// Combines main-buffer lines [0, StartLine) + overlay content + main-buffer lines [EndLine+1, end).
func (o *DiffOverlay) MergedContent(mainEd *editor.Editor) string {
	var parts []string

	for i := 0; i < o.StartLine && i < mainEd.LineCount(); i++ {
		parts = append(parts, mainEd.LineText(i))
	}

	for i := 0; i < o.Editor.LineCount(); i++ {
		parts = append(parts, o.Editor.LineText(i))
	}

	for i := o.EndLine + 1; i < mainEd.LineCount(); i++ {
		parts = append(parts, mainEd.LineText(i))
	}

	return strings.Join(parts, "\n")
}
