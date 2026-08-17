package editor

import "strings"

// DiffResult describes an inline diff preview for a proposed search-and-replace edit.
// StartLine..EndLine are the buffer lines that would be removed (0-indexed, inclusive).
// NewLines are the full lines that would replace them after the edit is applied.
type DiffResult struct {
	// StartLine is the first buffer line removed by the edit (0-indexed).
	StartLine int
	// EndLine is the last buffer line removed by the edit (0-indexed, inclusive).
	EndLine int
	// NewLines are the full replacement lines for StartLine..EndLine.
	NewLines []string
}

// ComputeDiff locates the search text in the buffer and computes the diff preview
// that would result from replacing it with replace. Returns nil if the search text
// is not found exactly once (same precondition as ApplyEdit).
func (e *Editor) ComputeDiff(search, replace string) *DiffResult {
	content := e.Buf.Content()
	if strings.Count(content, search) != 1 {
		return nil
	}

	idx := strings.Index(content, search)

	// Find the start line of the match using rune iteration.
	startLine := 0
	for _, r := range content[:idx] {
		if r == '\n' {
			startLine++
		}
	}

	// Find the end line of the match.
	endLine := startLine
	matchEnd := idx + len(search)
	for _, r := range content[idx:matchEnd] {
		if r == '\n' {
			endLine++
		}
	}

	// Extract the prefix (before match on first line) and suffix (after match on last line).
	lineStart := strings.LastIndex(content[:idx], "\n") + 1 // 0 if no newline before
	lineEnd := strings.Index(content[matchEnd:], "\n")
	if lineEnd == -1 {
		lineEnd = len(content)
	} else {
		lineEnd = matchEnd + lineEnd
	}

	prefix := content[lineStart:idx]
	suffix := content[matchEnd:lineEnd]

	// Build the replacement lines (full lines including prefix/suffix context).
	newText := prefix + replace + suffix
	newLines := strings.Split(newText, "\n")

	return &DiffResult{
		StartLine: startLine,
		EndLine:   endLine,
		NewLines:  newLines,
	}
}
