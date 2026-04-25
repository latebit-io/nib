package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/llm"
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
}

// NewReplaceFileTool creates a ReplaceFileTool with the given
// dependencies. The cache mirrors edit_file's so reads of the
// pre-replace content benefit from any earlier read_file calls.
func NewReplaceFileTool(ws FileReader, cache *FileCache) *ReplaceFileTool {
	return &ReplaceFileTool{workspace: ws, cache: cache}
}

// Definition returns the OpenAI-compatible tool schema for replace_file.
func (t *ReplaceFileTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name: "replace_file",
			Description: "Replace the ENTIRE contents of an existing file with new content in one operation. " +
				"Use this for wholesale rewrites that would require many fragile edit_file calls — extracting a module " +
				"into a clean shape, swapping a placeholder implementation for the real one, regenerating a generated file. " +
				"The proposal goes through the same approval and validation flow as edit_file (architecture caps, lint, " +
				"developer review at lower autonomy levels). Errors when the file does not exist — use write_file to create.",
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

// Execute validates the request and returns an EditProposal whose
// Search is the existing file content and Replace is the new content.
// Constructing the proposal this way means the existing approval /
// validation pipeline (which expects a search/replace pair) sees a
// uniformly-shaped edit and produces a meaningful "every-line
// changed" diff overlay for review.
func (t *ReplaceFileTool) Execute(_ context.Context, call llm.ToolCall) ToolResult {
	var args replaceArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return textResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	if args.Path == "" {
		return textResult("Error: path is required")
	}
	if len(args.Content) > maxDiffInputBytes {
		return textResult(fmt.Sprintf(
			"Error: content too large (%d bytes, max %d). Split the file or use multiple edit_file calls.",
			len(args.Content), maxDiffInputBytes))
	}

	canon := t.workspace.CanonPath(args.Path)
	if !inProject(t.workspace, canon) {
		return textResult(fmt.Sprintf("Error: %s is outside the project root", args.Path))
	}

	// Read existing content via cache (populated by prior read_file
	// calls or by the continueCh path that snapshots buffer content
	// on the TUI goroutine after every approved edit). Reading the
	// editor buffer directly from the agent goroutine would race
	// against TUI-owned buffer mutations — the existing architecture
	// avoids that by keeping buffer reads on the TUI goroutine and
	// pushing snapshots to the agent via channels. The cache is in
	// sync between agent turns; rare developer-typing-during-run
	// divergence is not yet addressed.
	existing, err := t.cache.LoadOrRead(canon, func() (string, error) {
		return t.workspace.ReadFile(args.Path)
	})
	if err != nil {
		// Distinguish missing-file from other I/O failures so the LLM
		// gets a clear "use write_file" steer rather than a generic
		// error it will probably retry endlessly.
		if errors.Is(err, fs.ErrNotExist) {
			return textResult(fmt.Sprintf(
				"Error: %s does not exist. Use write_file to create new files; replace_file is for existing files only.",
				args.Path))
		}
		return textResult(fmt.Sprintf("Error: cannot read %s: %v", args.Path, err))
	}
	if len(existing) > maxDiffInputBytes {
		t.cache.Invalidate(canon)
		return textResult(fmt.Sprintf(
			"Error: existing file too large (%d bytes, max %d) — cannot diff",
			len(existing), maxDiffInputBytes))
	}

	if existing == args.Content {
		// No-op rewrites are noise — the LLM occasionally proposes
		// these when it's "regenerating" an already-correct file.
		// Fail loud so the LLM knows nothing happened, not silently
		// success-appearing.
		return textResult(fmt.Sprintf(
			"Error: %s already has exactly the requested content; nothing to replace.",
			args.Path))
	}

	return ToolResult{
		Effect: EffectEditProposed,
		Payload: EditProposal{
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
		},
	}
}
