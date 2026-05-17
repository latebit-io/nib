package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
)

// ReplaceFileTool replaces the entire contents of an existing file in
// one shot, routing through the same approval/validation flow as
// edit_file. Unlike edit_file (search/replace, breaks down for large
// blocks because exact-match across hundreds of lines is fragile) and
// write_file (create-only by design), replace_file targets the
// "wholesale rewrite" case directly — the LLM no longer has to dance
// around the tool surface with shrinking anchors or request_input
// asking for permission to "use a different strategy."
//
// Single responsibility: replace existing files only. Calls against a
// non-existent path return an error pointing at write_file. The
// approval flow surfaces the diff as an "every-line replaced" overlay;
// the validator pipeline runs against the full After content (so the
// architecture cap, lint stage, etc., still gate). Path-traversal
// guard, project-root containment, and size cap match edit_file.
type ReplaceFileTool struct {
	workspace FileReader
	cache     *FileCache
	approver  Approver
}

// NewReplaceFileTool creates a ReplaceFileTool with the given
// dependencies. The cache mirrors edit_file's so reads of the
// pre-replace content benefit from any earlier read_file calls; the
// approver mediates the same approval flow.
func NewReplaceFileTool(ws FileReader, cache *FileCache, approver Approver) *ReplaceFileTool {
	return &ReplaceFileTool{workspace: ws, cache: cache, approver: approver}
}

// Definition returns the OpenAI-compatible tool schema for replace_file.
func (t *ReplaceFileTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "replace_file",
			Description: "Rewrite an existing file's entire contents in one operation. Use for wholesale rewrites where many edit_file calls would be fragile (extracting a module, swapping placeholder for real impl, regenerating). Goes through the same approval+validation as edit_file. Errors if file does not exist — use write_file instead.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"path": {
						Type:        "string",
						Description: "Project-relative path to the existing file.",
					},
					"content": {
						Type:        "string",
						Description: "Full new content of the file. Replaces everything currently in the file.",
					},
					"reason": {
						Type: "string",
						Description: "One-sentence explanation of why a wholesale rewrite is the right move here, " +
							"surfaced to the developer in the diff overlay header.",
					},
				},
				Required: []string{"path", "content"},
			},
		},
	}
}

type replaceArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Reason  string `json:"reason"`
}

// Execute validates the request and submits an [EditProposal] through
// the configured [Approver]. Search is the existing file content
// and Replace is the new content; constructing the proposal this way
// means the existing approval/validation pipeline (which expects a
// search/replace pair) sees a uniformly-shaped edit and produces a
// meaningful "every-line changed" diff overlay for review.
func (t *ReplaceFileTool) Execute(ctx context.Context, call llm.ToolCall) ToolResult {
	var args replaceArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return errorResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	if args.Path == "" {
		return errorResult("Error: path is required")
	}
	if len(args.Content) > maxDiffInputBytes {
		return errorResult(fmt.Sprintf(
			"Error: content too large (%d bytes, max %d). Split the file or use multiple edit_file calls.",
			len(args.Content), maxDiffInputBytes))
	}

	canon := t.workspace.CanonPath(args.Path)
	if !inProject(t.workspace, canon) {
		return errorResult(fmt.Sprintf("Error: %s is outside the project root", args.Path))
	}

	// Read existing content via cache (populated by prior read_file
	// calls or seeded by the orchestrator after every approved edit
	// with the proposal's expected content). Reading the editor
	// buffer directly from the agent goroutine would race against
	// TUI-owned buffer mutations — the existing architecture avoids
	// that by keeping buffer reads on the TUI goroutine. The cache
	// is in sync between agent turns.
	existing, err := t.cache.LoadOrRead(canon, func() (string, error) {
		return t.workspace.ReadFile(args.Path)
	})
	if err != nil {
		// Distinguish missing-file from other I/O failures so the LLM
		// gets a clear "use write_file" steer rather than a generic
		// error it will probably retry endlessly.
		if errors.Is(err, fs.ErrNotExist) {
			return errorResult(fmt.Sprintf(
				"Error: %s does not exist. Use write_file to create new files; replace_file is for existing files only.",
				args.Path))
		}
		return errorResult(fmt.Sprintf("Error: cannot read %s: %v", args.Path, err))
	}
	if len(existing) > maxDiffInputBytes {
		t.cache.Invalidate(canon)
		return errorResult(fmt.Sprintf(
			"Error: existing file too large (%d bytes, max %d) — cannot diff",
			len(existing), maxDiffInputBytes))
	}

	if existing == args.Content {
		// No-op rewrites are noise — the LLM occasionally proposes
		// these when it's "regenerating" an already-correct file.
		// Fail loud so the LLM knows nothing happened, not silently
		// success-appearing.
		return errorResult(fmt.Sprintf(
			"Error: %s already has exactly the requested content; nothing to replace.",
			args.Path))
	}

	body, isErr := t.approver.Propose(ctx, EditProposal{
		Edit: event.PendingEdit{
			ID:      call.ID,
			Path:    args.Path,
			Search:  existing,
			Replace: args.Content,
			Reason:  args.Reason,
		},
		Path:            args.Path,
		CanonPath:       canon,
		ExpectedContent: args.Content,
		TouchedLines:    changedLineRanges(existing, args.Content),
	})
	return ToolResult{Content: body, IsError: isErr}
}
