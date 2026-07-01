// Package openfile provides a frontend-agnostic open-file handle.
//
// An OpenFile pairs a [buffer.Buffer] with edit-domain operations (search,
// replace, diff preview) and persistence. It carries no UI state — no
// cursor, no selection, no viewport, no highlighter. Consumers that need
// rendering state (the TUI editor controller) wrap an OpenFile and add
// their own state on top; consumers that only need buffer content (the
// agent's Workspace path, headless binaries, the LSP bridge) can hold an
// OpenFile directly without linking against a renderer.
//
// Edit operations that would have placed a cursor in a UI controller
// instead return an [EditOutcome] describing where the cursor *would* land.
// The caller decides whether to forward that to a TUI, emit a navigate
// event, or ignore it.
package openfile

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/latebit-io/nib/engine/buffer"
)

// LineOrigin is the provenance of a buffer line (developer vs agent).
// Re-exported from the buffer package so consumers depend on openfile only.
type LineOrigin = buffer.Origin

// OriginDeveloper marks a line the developer wrote (or loaded from disk).
const OriginDeveloper = buffer.OriginDeveloper

// OriginAgent marks a line the agent wrote (set after edit approval).
const OriginAgent = buffer.OriginAgent

// OpenFile is a frontend-agnostic handle to a file open in the session.
// Holds a [buffer.Buffer] and exposes edit-domain operations that operate
// on buffer content alone. UI state (cursor, selection, scroll, viewport,
// highlighter) lives on the consumer that wraps this handle.
type OpenFile struct {
	// Buf is the underlying buffer. Public so consumers can perform
	// buffer-level operations without an extra accessor layer; OpenFile
	// adds edit-domain operations and the persistence boundary, not a
	// thicker wrapper.
	Buf *buffer.Buffer
}

// New constructs an OpenFile around the given buffer. The buffer must
// not be nil; callers are responsible for constructing the buffer (e.g.
// via [buffer.New] or [buffer.NewFromFile]).
func New(buf *buffer.Buffer) *OpenFile {
	return &OpenFile{Buf: buf}
}

// Path returns the file path associated with the underlying buffer.
// Empty for in-memory buffers that have never been bound to disk.
func (o *OpenFile) Path() string { return o.Buf.Path }

// Modified reports whether the buffer has unsaved changes.
func (o *OpenFile) Modified() bool { return o.Buf.Modified }

// Content returns the full buffer content as a single string.
func (o *OpenFile) Content() string { return o.Buf.Content() }

// Save writes the buffer to disk. Returns nil on success.
func (o *OpenFile) Save() error { return o.Buf.Save() }

// EditLocation describes a position in the buffer (0-indexed line and
// rune column).
type EditLocation struct {
	Line int // 0-indexed line
	Col  int // 0-indexed rune column
}

// EditOutcome reports the result of [OpenFile.ApplyEdit]. When Applied is
// true, NewCursor is the buffer position where a UI consumer should place
// its cursor (the start of the inserted replacement). When Applied is
// false, FailureReason carries the human-readable error.
type EditOutcome struct {
	Applied       bool         // true when the edit mutated the buffer
	NewCursor     EditLocation // start of the replacement; meaningful only when Applied
	FailureReason string       // human-readable reason; set only when !Applied
}

// LocateEdit finds the unique occurrence of search in the buffer.
// Returns the location and "" on success, or a zero EditLocation and a
// reason on failure (zero matches or more than one).
func (o *OpenFile) LocateEdit(search string) (EditLocation, string) {
	content := o.Buf.Content()
	count := strings.Count(content, search)
	switch count {
	case 1:
		idx := strings.Index(content, search)
		line, col := 0, 0
		for _, r := range content[:idx] {
			if r == '\n' {
				line++
				col = 0
			} else {
				col++
			}
		}
		return EditLocation{Line: line, Col: col}, ""
	case 0:
		return EditLocation{}, "Edit could not be applied — text not found"
	default:
		return EditLocation{}, fmt.Sprintf("Edit could not be applied — %d matches found, expected 1", count)
	}
}

// ApplyEdit applies a search-and-replace edit to the buffer. The edit is
// grouped for undo. Per-line origin annotations on the replacement text
// are applied via lineOrigins (index 0 = first replacement line; nil
// entry means "leave this line's origin alone").
//
// Unlike a UI controller, ApplyEdit does not move a cursor — it returns
// the location where a cursor should be placed in the [EditOutcome].
// Callers that drive a UI translate this into a navigate signal.
func (o *OpenFile) ApplyEdit(search, replace string, lineOrigins []*LineOrigin) EditOutcome {
	loc, reason := o.LocateEdit(search)
	if reason != "" {
		return EditOutcome{Applied: false, FailureReason: reason}
	}
	searchRunes := utf8.RuneCountInString(search)
	o.Buf.BeginGroup()
	o.Buf.Delete(loc.Line, loc.Col, searchRunes)
	o.Buf.Insert(loc.Line, loc.Col, replace)
	for i, origin := range lineOrigins {
		if origin != nil {
			o.Buf.SetLineOrigin(loc.Line+i, *origin)
		}
	}
	o.Buf.EndGroup()
	return EditOutcome{Applied: true, NewCursor: loc}
}

// ReplaceRange replaces the rune range [line, col .. line, col+length)
// with text, in a single undo group. Buffer-level operation: it does not
// touch any selection or cursor state. UI consumers that drive a
// find-and-replace flow handle their own selection/cursor placement.
func (o *OpenFile) ReplaceRange(line, col, length int, text string) {
	o.Buf.BeginGroup()
	defer o.Buf.EndGroup()
	o.Buf.Delete(line, col, length)
	o.Buf.Insert(line, col, text)
}

// DiffResult describes an inline diff preview for a proposed
// search-and-replace edit. StartLine..EndLine are the buffer lines that
// would be removed (0-indexed, inclusive). NewLines are the full lines
// that would replace them after the edit is applied.
type DiffResult struct {
	StartLine int      // first buffer line removed (0-indexed)
	EndLine   int      // last buffer line removed (inclusive)
	NewLines  []string // full replacement lines
}

// ComputeDiff locates the search text in the buffer and computes the
// diff preview that would result from replacing it with replace. Returns
// nil if the search text is not found exactly once (same precondition
// as ApplyEdit).
func (o *OpenFile) ComputeDiff(search, replace string) *DiffResult {
	content := o.Buf.Content()
	if strings.Count(content, search) != 1 {
		return nil
	}

	idx := strings.Index(content, search)

	startLine := 0
	for _, r := range content[:idx] {
		if r == '\n' {
			startLine++
		}
	}

	endLine := startLine
	matchEnd := idx + len(search)
	for _, r := range content[idx:matchEnd] {
		if r == '\n' {
			endLine++
		}
	}

	lineStart := strings.LastIndex(content[:idx], "\n") + 1
	lineEnd := strings.Index(content[matchEnd:], "\n")
	if lineEnd == -1 {
		lineEnd = len(content)
	} else {
		lineEnd = matchEnd + lineEnd
	}

	prefix := content[lineStart:idx]
	suffix := content[matchEnd:lineEnd]

	newText := prefix + replace + suffix
	newLines := strings.Split(newText, "\n")

	return &DiffResult{
		StartLine: startLine,
		EndLine:   endLine,
		NewLines:  newLines,
	}
}
