package agent

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/junto/engine/llm"
)

// missingFileWorkspace returns fs.ErrNotExist for any ReadFile call.
// Used to exercise replace_file's "use write_file instead" steer
// without standing up a real filesystem fixture.
type missingFileWorkspace struct{ *testWorkspace }

func (w *missingFileWorkspace) ReadFile(_ string) (string, error) {
	return "", fs.ErrNotExist
}

// TestReplaceFileTool_ReturnsEditProposal verifies the happy path:
// existing file, new content, valid args → EditProposal whose Search
// is the existing content and Replace is the new content. The
// existing approval/validation pipeline (which expects a
// search/replace pair) sees a uniformly-shaped edit and produces a
// meaningful "every-line replaced" diff overlay.
func TestReplaceFileTool_ReturnsEditProposal(t *testing.T) {
	t.Parallel()

	const oldContent = "local x = 1\nlocal y = 2\n"
	const newContent = "local x = 99\nlocal y = 88\nlocal z = 77\n"
	ws := &testWorkspace{
		files:     map[string]string{"src/main.lua": oldContent},
		inContext: map[string]bool{},
	}
	tool := NewReplaceFileTool(ws, NewFileCache())

	args := mustMarshal(t, replaceArgs{
		Path:    "src/main.lua",
		Content: newContent,
		Reason:  "swap placeholder for real implementation",
	})
	call := llm.ToolCall{
		ID:       "rp-1",
		Function: llm.FunctionCall{Name: "replace_file", Arguments: string(args)},
	}

	result := tool.Execute(context.Background(), call)

	if result.Effect != EffectEditProposed {
		t.Fatalf("Effect = %d, want EffectEditProposed", result.Effect)
	}
	proposal, ok := result.Payload.(EditProposal)
	if !ok {
		t.Fatalf("Payload type = %T, want EditProposal", result.Payload)
	}
	if proposal.Edit.Search != oldContent {
		t.Errorf("Edit.Search = %q, want existing content", proposal.Edit.Search)
	}
	if proposal.Edit.Replace != newContent {
		t.Errorf("Edit.Replace = %q, want new content", proposal.Edit.Replace)
	}
	if proposal.ExpectedContent != newContent {
		t.Errorf("ExpectedContent = %q, want new content", proposal.ExpectedContent)
	}
	if proposal.Edit.ID != call.ID {
		t.Errorf("Edit.ID = %q, want %q", proposal.Edit.ID, call.ID)
	}
}

// TestReplaceFileTool_MissingFileSteersToWriteFile verifies the
// dedicated "file does not exist" error message — single
// responsibility means replace_file errors instead of falling back
// to creation, and the error explicitly tells the LLM to use
// write_file. Without this, the LLM would retry endlessly trying to
// "fix" the call.
func TestReplaceFileTool_MissingFileSteersToWriteFile(t *testing.T) {
	t.Parallel()

	ws := &missingFileWorkspace{
		testWorkspace: &testWorkspace{inContext: map[string]bool{}},
	}
	tool := NewReplaceFileTool(ws, NewFileCache())

	args := mustMarshal(t, replaceArgs{
		Path:    "src/new.lua",
		Content: "x",
	})
	result := tool.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{Name: "replace_file", Arguments: string(args)},
	})
	if result.Effect != EffectNone {
		t.Fatalf("Effect = %d, want EffectNone (text error)", result.Effect)
	}
	if !strings.Contains(result.Content, "does not exist") {
		t.Errorf("error missing 'does not exist': %q", result.Content)
	}
	if !strings.Contains(result.Content, "write_file") {
		t.Errorf("error missing 'write_file' steer: %q", result.Content)
	}
}

// TestReplaceFileTool_NoOpRewriteFailsLoud verifies that requesting a
// replacement with content identical to the existing file returns an
// error. The LLM occasionally proposes these when "regenerating" an
// already-correct file; silent success would mask the wasted call
// and inflate the session's edit count without actually changing
// anything.
func TestReplaceFileTool_NoOpRewriteFailsLoud(t *testing.T) {
	t.Parallel()

	const same = "local same = true\n"
	ws := &testWorkspace{
		files:     map[string]string{"main.lua": same},
		inContext: map[string]bool{},
	}
	tool := NewReplaceFileTool(ws, NewFileCache())

	args := mustMarshal(t, replaceArgs{
		Path:    "main.lua",
		Content: same,
	})
	result := tool.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{Name: "replace_file", Arguments: string(args)},
	})
	if result.Effect != EffectNone {
		t.Fatalf("Effect = %d, want EffectNone (text error)", result.Effect)
	}
	if !strings.Contains(result.Content, "already has exactly the requested content") {
		t.Errorf("error missing no-op message: %q", result.Content)
	}
}

// TestReplaceFileTool_ValidatesPath verifies argument shape errors
// (missing path) and project-root containment.
func TestReplaceFileTool_ValidatesPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		path       string
		root       string
		wantSubstr string
	}{
		{
			name:       "empty path",
			path:       "",
			wantSubstr: "path is required",
		},
		{
			name:       "outside project root",
			path:       "/etc/passwd",
			root:       filepath.FromSlash("/safe/project"),
			wantSubstr: "outside the project root",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := &testWorkspace{
				root:      tc.root,
				files:     map[string]string{},
				inContext: map[string]bool{},
			}
			tool := NewReplaceFileTool(ws, NewFileCache())
			args := mustMarshal(t, replaceArgs{Path: tc.path, Content: "x"})
			result := tool.Execute(context.Background(), llm.ToolCall{
				Function: llm.FunctionCall{Name: "replace_file", Arguments: string(args)},
			})
			if !strings.Contains(result.Content, tc.wantSubstr) {
				t.Errorf("error missing %q: %q", tc.wantSubstr, result.Content)
			}
			if result.Effect != EffectNone {
				t.Errorf("Effect = %d, want EffectNone", result.Effect)
			}
		})
	}
}

// TestReplaceFileTool_DefinitionAdvertisesSchema verifies the tool
// description names the use case (wholesale rewrites) and the
// fallback (write_file) so the LLM picks correctly between the
// three file tools.
func TestReplaceFileTool_DefinitionAdvertisesSchema(t *testing.T) {
	t.Parallel()

	tool := NewReplaceFileTool(&testWorkspace{inContext: map[string]bool{}}, NewFileCache())
	def := tool.Definition()

	if def.Function.Name != "replace_file" {
		t.Errorf("Name = %q, want replace_file", def.Function.Name)
	}
	for _, want := range []string{"wholesale", "write_file", "edit_file"} {
		if !strings.Contains(def.Function.Description, want) {
			t.Errorf("description missing %q: %q", want, def.Function.Description)
		}
	}
	for _, required := range []string{"path", "content"} {
		var found bool
		for _, r := range def.Function.Parameters.Required {
			if r == required {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Required missing %q: %v", required, def.Function.Parameters.Required)
		}
	}
}

// bufferAwareWorkspace adds a CurrentContent override on top of the
// base testWorkspace, simulating an open editor whose buffer holds
// content ahead of (or different from) the disk-backed ReadFile
// result. This is the regression scenario: prior edit_file calls in
// the same agent turn applied to the buffer but the cache/disk view
// still holds the stale pre-edit content.
type bufferAwareWorkspace struct {
	*testWorkspace
	bufferContent map[string]string // path → live buffer content
}

func (w *bufferAwareWorkspace) CurrentContent(path string) (string, error) {
	if c, ok := w.bufferContent[path]; ok {
		return c, nil
	}
	return w.ReadFile(path)
}

// TestReplaceFileTool_PrefersBufferOverDisk verifies the
// CurrentContentReader path: when the workspace exposes buffer
// content, replace_file's Search is built from the buffer (live
// state) rather than disk (cache-lagged state). Without this fix
// the TUI's search-and-replace match against the buffer fails for
// any file the agent edited earlier in the same turn — exactly the
// "[edit could not be matched — auto-rejecting]" path the Pac-Man
// rerun surfaced.
func TestReplaceFileTool_PrefersBufferOverDisk(t *testing.T) {
	t.Parallel()

	const diskContent = "-- stale on-disk version\nlocal x = 1\n"
	const bufferContent = "-- live buffer ahead of disk\nlocal x = 2\nlocal y = 3\n"
	const newContent = "-- replacement\nlocal x = 99\n"

	ws := &bufferAwareWorkspace{
		testWorkspace: &testWorkspace{
			files:     map[string]string{"src/main.lua": diskContent},
			inContext: map[string]bool{},
		},
		bufferContent: map[string]string{"src/main.lua": bufferContent},
	}
	tool := NewReplaceFileTool(ws, NewFileCache())

	args := mustMarshal(t, replaceArgs{Path: "src/main.lua", Content: newContent})
	result := tool.Execute(context.Background(), llm.ToolCall{
		ID:       "rp",
		Function: llm.FunctionCall{Name: "replace_file", Arguments: string(args)},
	})

	prop, ok := result.Payload.(EditProposal)
	if !ok {
		t.Fatalf("Payload type = %T, want EditProposal", result.Payload)
	}
	if prop.Edit.Search != bufferContent {
		t.Errorf("Search built from disk (%q), want buffer (%q)",
			prop.Edit.Search, bufferContent)
	}
}

// TestReplaceFileTool_FallsBackToCacheWithoutCurrentContentReader
// verifies the fallback path: a workspace that doesn't implement
// CurrentContentReader (e.g. a test stub or a future external
// frontend) gets the original cache+disk behaviour. This keeps the
// new capability optional rather than breaking existing callers.
func TestReplaceFileTool_FallsBackToCacheWithoutCurrentContentReader(t *testing.T) {
	t.Parallel()

	const diskContent = "old content\n"
	ws := &testWorkspace{
		files:     map[string]string{"main.lua": diskContent},
		inContext: map[string]bool{},
	}
	tool := NewReplaceFileTool(ws, NewFileCache())

	args := mustMarshal(t, replaceArgs{Path: "main.lua", Content: "new content"})
	result := tool.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{Name: "replace_file", Arguments: string(args)},
	})
	prop, ok := result.Payload.(EditProposal)
	if !ok {
		t.Fatalf("Payload type = %T, want EditProposal", result.Payload)
	}
	if prop.Edit.Search != diskContent {
		t.Errorf("Search = %q, want %q (disk fallback)", prop.Edit.Search, diskContent)
	}
}

// TestReplaceFileTool_PendingEditCarriesReason verifies the
// developer-facing reason flows through to the diff overlay header
// — surfacing why a wholesale rewrite was the right move at this
// point.
func TestReplaceFileTool_PendingEditCarriesReason(t *testing.T) {
	t.Parallel()

	ws := &testWorkspace{
		files:     map[string]string{"x.lua": "old"},
		inContext: map[string]bool{},
	}
	tool := NewReplaceFileTool(ws, NewFileCache())

	const reason = "swap placeholder ghost AI for arcade-accurate behavior"
	args := mustMarshal(t, replaceArgs{Path: "x.lua", Content: "new", Reason: reason})
	result := tool.Execute(context.Background(), llm.ToolCall{
		ID:       "rp",
		Function: llm.FunctionCall{Name: "replace_file", Arguments: string(args)},
	})
	prop, ok := result.Payload.(EditProposal)
	if !ok {
		t.Fatalf("Payload type = %T, want EditProposal", result.Payload)
	}
	if prop.Edit.Reason != reason {
		t.Errorf("Edit.Reason = %q, want %q", prop.Edit.Reason, reason)
	}
}
