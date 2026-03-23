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

func (t *EditFileTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "edit_file",
			Description: "Search for exact text in a file and replace it. The search string must match the file content exactly (including whitespace and newlines). To delete text, set replace to an empty string. To insert, include anchor text in search and repeat it in replace with the new code added.",
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

func (t *EditFileTool) Execute(ctx context.Context, call llm.ToolCall) string {
	var args editArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("Error: invalid arguments: %v", err)
	}
	if args.Path == "" {
		return "Error: path is required"
	}

	// Get file content from cache or workspace
	canon := t.workspace.CanonPath(args.Path)
	content, ok := t.cache.Get(canon)
	if !ok {
		var err error
		content, err = t.workspace.ReadFile(args.Path)
		if err != nil {
			return fmt.Sprintf("Error: cannot read %s: %v", args.Path, err)
		}
		t.cache.Set(canon, content)
	}

	// Empty search is only valid when the file is empty (insert into empty file).
	// Otherwise the agent must provide text to match.
	if args.Search == "" {
		if content != "" {
			return "Error: search field cannot be empty (file is not empty — copy existing text to anchor your edit)"
		}
		// Empty file, empty search → treat as full-file replacement (insert).
		// Set search to content (empty string) so the replace logic works.
	}

	// Silent retry: validate search text before presenting to user.
	matchCount := strings.Count(content, args.Search)
	if matchCount != 1 {
		t.mu.Lock()
		if t.silentRetries < t.maxSilentRetries {
			t.silentRetries++
			remaining := t.maxSilentRetries - t.silentRetries
			attempt := t.silentRetries
			t.mu.Unlock()
			var errMsg string
			if matchCount == 0 {
				errMsg = "search text not found in file"
			} else {
				errMsg = fmt.Sprintf("search text matches %d locations (expected exactly 1) — make the search text more specific", matchCount)
			}
			slog.Info("edit_file: silent retry", "reason", errMsg,
				"attempt", attempt, "path", args.Path, "search_len", len(args.Search))
			return fmt.Sprintf("Error: %s. You have %d retries left. Read the file content carefully and copy the exact text.\n\nCurrent file (%s):\n\n%s",
				errMsg, remaining, args.Path, content)
		}
		t.silentRetries = 0
		t.mu.Unlock()
		slog.Warn("edit_file: validation failed after max retries",
			"matches", matchCount, "path", args.Path, "search_len", len(args.Search))
		return fmt.Sprintf("Error: search text validation failed after %d retries. Use read_file to re-read the file and copy the exact text.\n\nCurrent file (%s):\n\n%s",
			t.maxSilentRetries, args.Path, content)
	}

	t.mu.Lock()
	t.silentRetries = 0
	t.mu.Unlock()

	// Send proposed edit to frontend
	t.send(StatusEvent{Status: "waiting"})
	t.send(EditProposedEvent{Edit: PendingEdit{
		ID:      call.ID,
		Path:    args.Path,
		Search:  args.Search,
		Replace: args.Replace,
		Reason:  args.Reason,
	}})

	// Wait for approval or rejection
	select {
	case <-ctx.Done():
		return "Error: agent canceled"
	case approved := <-t.approveCh:
		if !approved {
			t.send(StatusEvent{Status: "thinking"})
			t.send(TokenEvent{Text: "\n[Edit rejected]\n\n"})
			// Re-read in case content changed
			if c, ok := t.cache.Get(canon); ok {
				content = c
			}
			return fmt.Sprintf("The developer rejected this edit. Try a different approach or move on.\n\nCurrent file (%s):\n\n%s",
				args.Path, content)
		}
	}

	// Approved — auto-add to context now that the edit is validated and accepted.
	if !t.workspace.InContext(args.Path) {
		t.workspace.AddContext(args.Path)
	}

	// Wait for user to finish editing and continue.
	expectedContent := strings.Replace(content, args.Search, args.Replace, 1)

	t.send(StatusEvent{Status: "editing"})
	t.send(TokenEvent{Text: "\n[Edit approved — waiting for continue]\n"})

	select {
	case <-ctx.Done():
		return "Error: agent canceled"
	case newContent := <-t.continueCh:
		t.cache.Set(canon, newContent)
		t.send(StatusEvent{Status: "thinking"})
		t.send(TokenEvent{Text: "\n"})

		if newContent != expectedContent {
			return fmt.Sprintf("Edit applied. The applied edit differs from what you proposed. "+
				"Study what changed — it signals the developer's intent. Recalibrate your approach to align with their direction. "+
				"If you notice a syntax error or bug in their edit, point it out and propose a fix — do not silently change it.\n\nCurrent file (%s):\n\n%s",
				args.Path, newContent)
		}
		return fmt.Sprintf("Edit applied successfully.\n\nCurrent file (%s):\n\n%s",
			args.Path, newContent)
	}
}
