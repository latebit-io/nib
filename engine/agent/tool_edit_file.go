package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

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
// It validates the search text, sends an approval event, and blocks
// until the developer approves or rejects.
type EditFileTool struct {
	workspace  Workspace
	cache      *FileCache
	approveCh  chan bool
	continueCh chan string
	send       func(Event)

	mu               sync.Mutex
	silentRetries    int
	maxSilentRetries int
}

// NewEditFileTool creates an EditFileTool with the given dependencies.
func NewEditFileTool(ws Workspace, cache *FileCache, approveCh chan bool, continueCh chan string, send func(Event)) *EditFileTool {
	return &EditFileTool{
		workspace:        ws,
		cache:            cache,
		approveCh:        approveCh,
		continueCh:       continueCh,
		send:             send,
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

// Execute runs the edit_file tool: validate, propose, wait for approval, wait for continue.
func (t *EditFileTool) Execute(ctx context.Context, call llm.ToolCall) string {
	var args editArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("Error: invalid arguments: %v", err)
	}
	if args.Path == "" {
		return "Error: path is required"
	}

	content, canon, err := t.resolveContent(args.Path)
	if err != nil {
		return fmt.Sprintf("Error: cannot read %s: %v", args.Path, err)
	}

	if args.Search == "" && content != "" {
		return "Error: search field cannot be empty (file is not empty — copy existing text to anchor your edit)"
	}

	if errMsg := t.validateSearchMatch(args.Path, args.Search, content); errMsg != "" {
		return errMsg
	}

	t.send(StatusEvent{Status: "waiting"})
	t.send(EditProposedEvent{Edit: PendingEdit{
		ID:      call.ID,
		Path:    args.Path,
		Search:  args.Search,
		Replace: args.Replace,
		Reason:  args.Reason,
	}})

	if msg, canceled := t.waitForApproval(ctx, canon, args.Path); canceled {
		return "Error: agent canceled"
	} else if msg != "" {
		return msg
	}

	if !t.workspace.InContext(args.Path) {
		t.workspace.AddContext(args.Path)
	}

	expectedContent := strings.Replace(content, args.Search, args.Replace, 1)
	return t.waitForContinue(ctx, canon, args.Path, expectedContent)
}

// resolveContent returns the file content and canonical path, reading from
// cache first and falling back to disk.
func (t *EditFileTool) resolveContent(path string) (content, canon string, err error) {
	canon = t.workspace.CanonPath(path)
	content, ok := t.cache.Get(canon)
	if ok {
		slog.Debug("edit_file: cache hit", "path", path, "content_len", len(content))
		return content, canon, nil
	}
	content, err = t.workspace.ReadFile(path)
	if err != nil {
		return "", canon, err
	}
	t.cache.Set(canon, content)
	slog.Debug("edit_file: read from disk", "path", path, "content_len", len(content))
	return content, canon, nil
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

// waitForApproval blocks until the developer approves or rejects the edit,
// or the context is canceled. Returns (rejectionMsg, false) on reject,
// ("", true) on cancel, ("", false) on approve.
func (t *EditFileTool) waitForApproval(ctx context.Context, canon, path string) (msg string, canceled bool) {
	select {
	case <-ctx.Done():
		return "", true
	case approved, ok := <-t.approveCh:
		if !ok {
			return "Error: approval channel closed", true
		}
		if approved {
			return "", false
		}
	}

	t.send(StatusEvent{Status: "thinking"})
	t.send(TokenEvent{Text: "\n[Edit rejected]\n\n"})

	content := ""
	if c, ok := t.cache.Get(canon); ok {
		content = c
	}
	return fmt.Sprintf("The developer rejected this edit. Try a different approach or move on.\n\nCurrent file (%s):\n\n%s",
		path, truncateForPreview(content)), false
}

// waitForContinue blocks until the developer finishes editing and presses
// continue, or the context is canceled. Compares the new content against
// the expected result to detect developer modifications.
func (t *EditFileTool) waitForContinue(ctx context.Context, canon, path, expectedContent string) string {
	t.send(StatusEvent{Status: "editing"})
	t.send(TokenEvent{Text: "\n[Edit approved — waiting for continue]\n"})

	select {
	case <-ctx.Done():
		return "Error: agent canceled"
	case newContent, ok := <-t.continueCh:
		if !ok {
			return "Error: continue channel closed"
		}
		t.cache.Set(canon, newContent)
		t.send(StatusEvent{Status: "thinking"})
		t.send(TokenEvent{Text: "\n"})

		if newContent != expectedContent {
			diff := simpleDiff(expectedContent, newContent)
			return fmt.Sprintf("Edit applied, but the developer modified your edit. "+
				"IMPORTANT: The file content below is the AUTHORITATIVE current state. "+
				"Do NOT use any earlier version of this file from the conversation — only use what is shown here.\n\n"+
				"Developer's changes (what they changed from your proposal):\n```diff\n%s\n```\n\n"+
				"Recalibrate: study the diff — it signals the developer's intent. "+
				"Align your next steps with their direction. "+
				"If you notice a syntax error or bug in their edit, point it out and propose a fix.\n\n"+
				"Current file (%s):\n```\n%s\n```",
				diff, path, truncateForPreview(newContent))
		}
		return fmt.Sprintf("Edit applied successfully.\n\nCurrent file (%s):\n\n%s",
			path, truncateForPreview(newContent))
	}
}
