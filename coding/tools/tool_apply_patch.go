package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/engine/patch"
)

// ApplyPatchTool applies a Codex-style multi-hunk patch envelope to
// one existing file. It closes the gap between [EditFileTool] (one
// search/replace per call) and [ReplaceFileTool] (full file content):
// multi-location changes inside a single file ship only the diff,
// which is substantially cheaper in tool-call argument bytes than
// either of the existing primitives.
//
// The tool parses the patch via [github.com/latebit-io/nib/engine/patch],
// resolves each hunk against the current file content (with optional
// `@@` anchor disambiguation), produces the post-edit content, and
// then routes through the existing [Approver] path with an
// EditProposal whose Search is the current content and whose Replace
// is the post-edit content. That keeps the validation pipeline,
// architecture caps, lint stage, and developer review identical to
// the replace_file flow — the patch is a parser layer in front of
// the same downstream machinery.
type ApplyPatchTool struct {
	workspace FileReader
	cache     *FileCache
	approver  Approver
}

// NewApplyPatchTool creates an ApplyPatchTool with the given
// dependencies. workspace and cache mirror edit_file/replace_file so
// reads benefit from prior read_file calls and post-approval cache
// seeding; approver mediates the same approval flow.
func NewApplyPatchTool(ws FileReader, cache *FileCache, approver Approver) *ApplyPatchTool {
	return &ApplyPatchTool{workspace: ws, cache: cache, approver: approver}
}

// Definition returns the OpenAI-compatible tool schema for
// apply_patch. The description names the use case (multi-hunk single
// file) and the alternatives (write_file for new files, replace_file
// for wholesale rewrites) so the LLM picks correctly between the
// four file-mutation primitives.
func (t *ApplyPatchTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name: "apply_patch",
			Description: "Apply a multi-hunk patch envelope to ONE existing file. Best when several non-adjacent " +
				"locations in the same file must change together (rename a function and update its call sites); ships " +
				"only the diff lines, not the whole file. For new files use write_file; for wholesale rewrites use " +
				"replace_file; for a single search/replace use edit_file. Format:\n\n" +
				"*** Begin Patch\n*** Update File: relative/path\n@@ optional anchor\n unchanged context line\n" +
				"-removed line\n+added line\n*** End Patch\n\n" +
				"Each hunk must include at least one context line (leading space) or one '-' line so the location is " +
				"anchored. The optional @@ anchor is matched as a substring; hunks then match exactly at or after the " +
				"anchor line. Hunks apply in order; on a mismatch the tool names which hunk failed so you can retry " +
				"only that one.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"patch": {
						Type:        "string",
						Description: "The full patch envelope, beginning with '*** Begin Patch' and ending with '*** End Patch'.",
					},
					"reason": {
						Type:        "string",
						Description: "One-sentence explanation of the change, surfaced to the developer in the diff overlay header.",
					},
				},
				Required: []string{"patch"},
			},
		},
	}
}

type applyPatchArgs struct {
	Patch  string `json:"patch"`
	Reason string `json:"reason"`
}

// Execute parses the patch, applies it against the current file
// content, and submits an [EditProposal] through the [Approver]. On
// any failure (parse error, missing file, unmatched hunk, ambiguous
// match) the tool returns an error result naming the specific
// problem so the LLM can retry with a fix.
func (t *ApplyPatchTool) Execute(ctx context.Context, call llm.ToolCall) ToolResult {
	if len(call.Function.Arguments) > maxToolArgsBytes {
		return errorResult(fmt.Sprintf("Error: arguments too large (%d bytes, max %d).",
			len(call.Function.Arguments), maxToolArgsBytes))
	}
	var args applyPatchArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return errorResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	if args.Patch == "" {
		return errorResult("Error: patch is required")
	}

	parsed, err := patch.Parse(args.Patch)
	if err != nil {
		// patch.ParseError already embeds the line number and (for
		// UnsupportedV2Error) the LLM-facing steer toward write_file,
		// so prepending "Error:" is all the tool layer needs to add.
		return errorResult("Error: " + err.Error())
	}
	// Parser guarantees one file on success — defence in depth in case
	// a future v2 path constructs a multi-file Patch and forgets to
	// rewire the tool layer.
	if len(parsed.Files) != 1 {
		return errorResult("Error: v1 apply_patch supports exactly one '*** Update File' section per call")
	}
	fp := parsed.Files[0]

	canon := t.workspace.CanonPath(fp.Path)
	if !inProject(t.workspace, canon) {
		return errorResult(fmt.Sprintf("Error: %s is outside the project root", fp.Path))
	}
	if collidesWithWorkTree(t.workspace, canon) {
		return workTreeCollisionError(fp.Path)
	}

	existing, err := t.cache.LoadOrRead(canon, func() (string, error) {
		return t.workspace.ReadFile(fp.Path)
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errorResult(fmt.Sprintf(
				"Error: %s does not exist. apply_patch updates existing files; use write_file to create new ones.",
				fp.Path))
		}
		return errorResult(fmt.Sprintf("Error: cannot read %s: %v", fp.Path, err))
	}
	if len(existing) > maxDiffInputBytes {
		t.cache.Invalidate(canon)
		return errorResult(fmt.Sprintf(
			"Error: existing file too large (%d bytes, max %d) — cannot apply patch",
			len(existing), maxDiffInputBytes))
	}

	updated, applyErr := applyFilePatch(existing, fp)
	if applyErr != nil {
		return errorResult("Error: " + applyErr.Error())
	}
	if updated == existing {
		return errorResult(fmt.Sprintf(
			"Error: patch matched but produced no change to %s; verify the -/+ lines actually differ.",
			fp.Path))
	}

	body, isErr := t.approver.Propose(ctx, EditProposal{
		Edit: event.PendingEdit{
			ID:      call.ID,
			Path:    fp.Path,
			Search:  existing,
			Replace: updated,
			Reason:  args.Reason,
		},
		Path:            fp.Path,
		CanonPath:       canon,
		ExpectedContent: updated,
	})
	return ToolResult{Content: body, IsError: isErr}
}

// applyFilePatch resolves every hunk in fp against content and
// returns the post-edit content. Hunks apply in order; each searches
// the file state produced by the prior hunks so line-number shifts
// from earlier edits never affect later hunks. Errors name the
// 1-indexed hunk that failed.
func applyFilePatch(content string, fp patch.FilePatch) (string, error) {
	// Preserve the original trailing-newline shape. strings.Split
	// produces a trailing "" when content ends in "\n"; strings.Join
	// reverses that exactly, so we just round-trip through Split/Join.
	lines := strings.Split(content, "\n")
	for i, hunk := range fp.Hunks {
		next, err := applyHunk(lines, hunk)
		if err != nil {
			return "", fmt.Errorf("hunk %d: %w", i+1, err)
		}
		lines = next
	}
	return strings.Join(lines, "\n"), nil
}

// applyHunk locates the hunk's expected lines (context + delete) in
// lines, optionally narrowed to start at or after a line containing
// Anchor as a substring, then returns the line slice with the
// expected range replaced by the hunk's replacement lines (context +
// insert). Returns an error on:
//
//   - empty match target (hunk has no context or delete lines — the
//     parser only requires one -/+ line, so a +-only hunk reaches us);
//   - anchor not found in the file;
//   - expected lines not found (after anchor narrowing);
//   - expected lines match more than once (ambiguous — LLM should add
//     more context or an @@ anchor).
func applyHunk(lines []string, h patch.Hunk) ([]string, error) {
	expected, replacement := splitHunkLines(h)
	if len(expected) == 0 {
		return nil, errors.New("hunk has only '+' lines; add at least one context line (leading space) to anchor the insertion")
	}

	start := 0
	if h.Anchor != "" {
		anchorIdx := findAnchorLine(lines, h.Anchor)
		if anchorIdx < 0 {
			return nil, fmt.Errorf("anchor %q not found in file", h.Anchor)
		}
		start = anchorIdx
	}

	matches := findMatches(lines, expected, start)
	switch len(matches) {
	case 0:
		if h.Anchor != "" {
			return nil, fmt.Errorf("expected lines did not match the file at or after anchor %q", h.Anchor)
		}
		return nil, errors.New("expected lines did not match the file — verify context and '-' lines are exact")
	case 1:
		// fall through to apply
	default:
		return nil, fmt.Errorf("expected lines match %d locations — add an @@ anchor or more context to disambiguate",
			len(matches))
	}

	matchStart := matches[0]
	matchEnd := matchStart + len(expected)
	out := make([]string, 0, len(lines)-len(expected)+len(replacement))
	out = append(out, lines[:matchStart]...)
	out = append(out, replacement...)
	out = append(out, lines[matchEnd:]...)
	return out, nil
}

// splitHunkLines projects the hunk's mixed line list into the two
// sequences applyHunk needs: expected is the part of the file the
// hunk wants to find (context + delete); replacement is what to put
// in its place (context + insert).
func splitHunkLines(h patch.Hunk) (expected, replacement []string) {
	expected = make([]string, 0, len(h.Lines))
	replacement = make([]string, 0, len(h.Lines))
	for _, l := range h.Lines {
		switch l.Kind {
		case patch.HunkContext:
			expected = append(expected, l.Text)
			replacement = append(replacement, l.Text)
		case patch.HunkDelete:
			expected = append(expected, l.Text)
		case patch.HunkInsert:
			replacement = append(replacement, l.Text)
		}
	}
	return expected, replacement
}

// findAnchorLine returns the index of the first line in lines whose
// content contains anchor as a substring, or -1. Substring rather
// than exact match keeps the anchor format permissive — the model
// can write `@@ func main` to point at `func main() {` without
// having to reproduce the punctuation.
func findAnchorLine(lines []string, anchor string) int {
	for i, l := range lines {
		if strings.Contains(l, anchor) {
			return i
		}
	}
	return -1
}

// findMatches returns every start index >= start where expected
// appears as a contiguous run inside lines. Match equality is
// exact-string per line, same discipline edit_file enforces.
func findMatches(lines, expected []string, start int) []int {
	if len(expected) == 0 || len(expected) > len(lines)-start {
		return nil
	}
	var out []int
	end := len(lines) - len(expected)
	for i := start; i <= end; i++ {
		ok := true
		for j, want := range expected {
			if lines[i+j] != want {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, i)
		}
	}
	return out
}
