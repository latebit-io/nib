package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/llm"
)

// maxContentPreview is the max bytes of file content included in error messages
// sent back to the LLM. Prevents unbounded message sizes for large files.
const maxContentPreview = 8 * 1024

// maxDiffPreview is the max bytes of diff output included in recalibration
// messages. Caps the simpleDiff output to avoid blowing token budgets.
const maxDiffPreview = 4 * 1024

// truncateForPreview returns content truncated for LLM context windows.
// The suffix hints the LLM how to retrieve the full content.
func truncateForPreview(content string) string {
	return truncateWithHint(content, "use read_file for full content")
}

// truncateWithHint truncates content at maxContentPreview with a custom hint.
func truncateWithHint(content, hint string) string {
	if len(content) <= maxContentPreview {
		return content
	}
	return content[:maxContentPreview] + "\n\n[... truncated — " + hint + "]"
}

// maxDiffInputBytes caps the combined input size to simpleDiff.
// Files beyond this threshold get a placeholder instead of a line-level diff.
const maxDiffInputBytes = 10 * 1024 * 1024

// simpleDiff produces a unified-diff-like comparison between expected and actual
// content, showing only the lines that differ. Output is capped at maxDiffPreview
// bytes to avoid blowing token budgets on large file changes.
func simpleDiff(expected, actual string) string {
	if len(expected)+len(actual) > maxDiffInputBytes {
		return "(diff omitted: content too large)"
	}
	expectedLines := strings.Split(expected, "\n")
	actualLines := strings.Split(actual, "\n")

	var b diffBuilder
	ei, ai := 0, 0
	for ei < len(expectedLines) || ai < len(actualLines) {
		if ei < len(expectedLines) && ai < len(actualLines) && expectedLines[ei] == actualLines[ai] {
			ei++
			ai++
			continue
		}
		matchAhead := findMatch(expectedLines, actualLines, ei, ai)
		if matchAhead.found {
			ei, ai = b.writeHunk(expectedLines, actualLines, ei, matchAhead.ei, ai, matchAhead.ai)
		} else {
			ei, ai = b.writeHunk(expectedLines, actualLines, ei, len(expectedLines), ai, len(actualLines))
		}
		if b.truncated {
			break
		}
	}

	result := b.buf.String()
	if b.truncated && result == "" {
		return "[... diff truncated]"
	}
	if result == "" {
		return "(whitespace-only changes)"
	}
	if b.truncated {
		result += "\n[... diff truncated]"
	}
	return result
}

// diffBuilder accumulates diff lines with a size cap.
type diffBuilder struct {
	buf       strings.Builder
	truncated bool
}

// writeLine appends a diff line if under the cap. Returns false if truncated.
func (d *diffBuilder) writeLine(prefix, line string) bool {
	entry := prefix + " " + line + "\n"
	if d.buf.Len()+len(entry) > maxDiffPreview {
		d.truncated = true
		return false
	}
	d.buf.WriteString(entry)
	return true
}

// writeHunk writes removed lines [ei:endE) and added lines [ai:endA).
// Returns the new ei, ai positions.
func (d *diffBuilder) writeHunk(expected, actual []string, ei, endE, ai, endA int) (int, int) {
	for ; ei < endE; ei++ {
		if !d.writeLine("-", expected[ei]) {
			return ei, ai
		}
	}
	for ; ai < endA; ai++ {
		if !d.writeLine("+", actual[ai]) {
			return ei, ai
		}
	}
	return ei, ai
}

type matchResult struct {
	found  bool
	ei, ai int
}

// findMatch scans ahead to find the next line where expected and actual re-sync.
// Limited lookahead to avoid O(n²) on large files.
func findMatch(expected, actual []string, ei, ai int) matchResult {
	const maxLookahead = 20
	limitE := min(ei+maxLookahead, len(expected))
	limitA := min(ai+maxLookahead, len(actual))

	for de := 0; de < limitE-ei; de++ {
		for da := 0; da < limitA-ai; da++ {
			if de == 0 && da == 0 {
				continue // skip current position
			}
			if expected[ei+de] == actual[ai+da] {
				return matchResult{found: true, ei: ei + de, ai: ai + da}
			}
		}
	}
	return matchResult{}
}

// EditFileTool lets the LLM propose search-and-replace edits to files.
// It validates the search text and returns an EditProposal for the agent
// loop to handle (sending events, blocking on approval/continue).
type EditFileTool struct {
	workspace FileReader
	cache     *FileCache

	mu               sync.Mutex
	silentRetries    int
	maxSilentRetries int
}

// NewEditFileTool creates an EditFileTool with the given dependencies.
func NewEditFileTool(ws FileReader, cache *FileCache) *EditFileTool {
	return &EditFileTool{
		workspace:        ws,
		cache:            cache,
		maxSilentRetries: 3,
	}
}

// Reset clears silent retry state between runs. Safe to call while
// a previous Execute() is unwinding after cancellation.
func (t *EditFileTool) Reset() {
	t.mu.Lock()
	t.silentRetries = 0
	t.mu.Unlock()
}

// Definition returns the tool definition for the LLM.
func (t *EditFileTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "edit_file",
			Description: "Search for exact text in a file and replace it. The search string must match the file content exactly (including whitespace and newlines). The replace string must be correctly formatted code with proper indentation matching the file's style — never collapse multiple lines onto one line. Keep search text as SHORT as possible — only include lines that actually change, plus minimal context to match uniquely. Do NOT rewrite entire functions when only a few lines change. To delete text, set replace to an empty string. To insert, include anchor text in search and repeat it in replace with the new code added.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"path": {
						Type:        "string",
						Description: "File path relative to project root.",
					},
					"search": {
						Type:        "string",
						Description: "Exact text to find in the file. Must match verbatim.",
					},
					"replace": {
						Type:        "string",
						Description: "Text to replace the search text with. Empty string to delete.",
					},
					"reason": {
						Type:        "string",
						Description: "Brief explanation of the change, shown to the developer.",
					},
				},
				Required: []string{"path", "search", "replace", "reason"},
			},
		},
	}
}

type editArgs struct {
	Path    string `json:"path"`
	Search  string `json:"search"`
	Replace string `json:"replace"`
	Reason  string `json:"reason"`
}

// Execute validates the edit and returns an EditProposal for the agent loop.
// The tool no longer blocks on channels or sends events — all orchestration
// is handled by the agent loop via the EffectEditProposed effect.
func (t *EditFileTool) Execute(_ context.Context, call llm.ToolCall) ToolResult {
	var args editArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return textResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	if args.Path == "" {
		return textResult("Error: path is required")
	}

	content, canon, err := t.resolveContent(args.Path)
	if err != nil {
		return textResult(fmt.Sprintf("Error: cannot read %s: %v", args.Path, err))
	}

	if args.Search == "" && content != "" {
		return textResult("Error: search field cannot be empty (file is not empty — copy existing text to anchor your edit)")
	}

	correctedSearch, errMsg := t.validateSearchMatch(args.Path, args.Search, content)
	if errMsg != "" {
		return textResult(errMsg)
	}
	if correctedSearch != "" {
		args.Search = correctedSearch
	}

	expectedContent := strings.Replace(content, args.Search, args.Replace, 1)

	return ToolResult{
		Content: "", // filled by the agent loop after approval/rejection
		Effect:  EffectEditProposed,
		Payload: EditProposal{
			Edit: event.PendingEdit{
				ID:      call.ID,
				Path:    args.Path,
				Search:  args.Search,
				Replace: args.Replace,
				Reason:  args.Reason,
			},
			Path:            args.Path,
			CanonPath:       canon,
			ExpectedContent: expectedContent,
		},
	}
}

// resolveContent returns the file content and canonical path, reading from
// cache first and falling back to disk. The closure captures the original
// relative path so ReadFile receives the documented relative-path input
// while the cache is keyed by canonical absolute path.
func (t *EditFileTool) resolveContent(path string) (content, canon string, err error) {
	canon = t.workspace.CanonPath(path)
	content, err = t.cache.LoadOrRead(canon, func() (string, error) {
		return t.workspace.ReadFile(path)
	})
	if err != nil {
		return "", canon, err
	}
	if len(content) > maxDiffInputBytes {
		t.cache.Invalidate(canon) // don't retain oversized files in cache
		return "", canon, fmt.Errorf("file too large (%d bytes, max %d)", len(content), maxDiffInputBytes)
	}
	return content, canon, nil
}

// validateSearchMatch checks that the search text appears exactly once in the
// file content. Returns (correctedSearch, "") on success — correctedSearch is
// non-empty when a whitespace-normalized fallback matched and the caller
// should use it instead of the original search text. Returns ("", errMsg)
// on failure, managing the silent retry counter.
func (t *EditFileTool) validateSearchMatch(path, search, content string) (string, string) {
	matchCount := strings.Count(content, search)
	if matchCount == 1 {
		t.mu.Lock()
		t.silentRetries = 0
		t.mu.Unlock()
		return "", ""
	}

	// Exact match failed — try whitespace-normalized fallback.
	// Normalizes leading whitespace on each line so tab/space mismatches
	// and wrong indentation depth don't block the edit.
	if matchCount == 0 {
		if corrected := fuzzyWhitespaceMatch(search, content); corrected != "" {
			slog.Info("edit_file: whitespace-normalized match", "path", path)
			t.mu.Lock()
			t.silentRetries = 0
			t.mu.Unlock()
			return corrected, ""
		}
	}

	t.mu.Lock()
	if t.silentRetries < t.maxSilentRetries {
		t.silentRetries++
		remaining := t.maxSilentRetries - t.silentRetries
		attempt := t.silentRetries
		t.mu.Unlock()

		errMsg := "search text not found in file"
		if matchCount > 1 {
			errMsg = fmt.Sprintf("search text matches %d locations (expected exactly 1) — make the search text more specific", matchCount)
		}
		slog.Info("edit_file: silent retry", "reason", errMsg,
			"attempt", attempt, "path", path, "search_len", len(search))
		return "", fmt.Sprintf("Error: %s. You have %d retries left. Read the file content carefully and copy the exact text.\n\nCurrent file (%s):\n\n%s",
			errMsg, remaining, path, truncateForPreview(content))
	}

	t.silentRetries = 0
	t.mu.Unlock()
	slog.Warn("edit_file: validation failed after max retries",
		"matches", matchCount, "path", path, "search_len", len(search))
	return "", fmt.Sprintf("Error: search text validation failed after %d retries. Use read_file to re-read the file and copy the exact text.\n\nCurrent file (%s):\n\n%s",
		t.maxSilentRetries, path, truncateForPreview(content))
}

// fuzzyWhitespaceMatch attempts to find the search text in content by
// normalizing leading whitespace on each line. If exactly one match is
// found, returns the actual text from the file that matched. Returns ""
// if no match or multiple matches.
func fuzzyWhitespaceMatch(search, content string) string {
	searchLines := strings.Split(search, "\n")
	if len(searchLines) == 0 {
		return ""
	}

	// Build a pattern from the search text with leading whitespace stripped.
	stripped := make([]string, len(searchLines))
	for i, line := range searchLines {
		stripped[i] = strings.TrimLeft(line, " \t")
	}

	// Scan content lines for a contiguous block that matches when
	// leading whitespace is stripped.
	contentLines := strings.Split(content, "\n")
	var matches []string
	for i := 0; i <= len(contentLines)-len(searchLines); i++ {
		match := true
		for j, want := range stripped {
			got := strings.TrimLeft(contentLines[i+j], " \t")
			if got != want {
				match = false
				break
			}
		}
		if match {
			actual := strings.Join(contentLines[i:i+len(searchLines)], "\n")
			matches = append(matches, actual)
		}
	}

	if len(matches) == 1 && matches[0] != search {
		return matches[0]
	}
	return ""
}
