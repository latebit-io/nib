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

// truncateForPreview returns content truncated for LLM error messages.
func truncateForPreview(content string) string {
	if len(content) <= maxContentPreview {
		return content
	}
	return content[:maxContentPreview] + "\n\n[... truncated — use read_file for full content]"
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

	if errMsg := t.validateSearchMatch(args.Path, args.Search, content); errMsg != "" {
		return textResult(errMsg)
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
// cache first and falling back to disk.
func (t *EditFileTool) resolveContent(path string) (content, canon string, err error) {
	canon = t.workspace.CanonPath(path)
	content, err = t.cache.LoadOrRead(canon, t.workspace.ReadFile)
	return content, canon, err
}

// validateSearchMatch checks that the search text appears exactly once in
// the file content. On failure it manages the silent retry counter and
// returns the error message to send back to the LLM. Returns "" on success.
func (t *EditFileTool) validateSearchMatch(path, search, content string) string {
	matchCount := strings.Count(content, search)
	if matchCount == 1 {
		t.mu.Lock()
		t.silentRetries = 0
		t.mu.Unlock()
		return ""
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
		return fmt.Sprintf("Error: %s. You have %d retries left. Read the file content carefully and copy the exact text.\n\nCurrent file (%s):\n\n%s",
			errMsg, remaining, path, truncateForPreview(content))
	}

	t.silentRetries = 0
	t.mu.Unlock()
	slog.Warn("edit_file: validation failed after max retries",
		"matches", matchCount, "path", path, "search_len", len(search))
	return fmt.Sprintf("Error: search text validation failed after %d retries. Use read_file to re-read the file and copy the exact text.\n\nCurrent file (%s):\n\n%s",
		t.maxSilentRetries, path, truncateForPreview(content))
}
