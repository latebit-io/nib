package tools

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
)

// missingFileWorkspace returns fs.ErrNotExist for any ReadFile call.
// Used to exercise replace_file's "use write_file instead" steer
// without standing up a real filesystem fixture.
type missingFileWorkspace struct{ *testWorkspace }

func (w *missingFileWorkspace) ReadFile(_ string) (string, error) {
	return "", fs.ErrNotExist
}

// TestReplaceFileTool_ReturnsEditProposal verifies the happy path:
// existing file, new content, valid args → an EditProposal flows to
// the [Approver] whose Search is the existing content and Replace is
// the new content. The existing approval/validation pipeline (which
// expects a search/replace pair) sees a uniformly-shaped edit and
// produces a meaningful "every-line replaced" diff overlay.
func TestReplaceFileTool_ReturnsEditProposal(t *testing.T) {
	t.Parallel()

	const oldContent = "local x = 1\nlocal y = 2\n"
	const newContent = "local x = 99\nlocal y = 88\nlocal z = 77\n"
	ws := &testWorkspace{
		files: map[string]string{"src/main.lua": oldContent},
	}
	app := &fakeApprover{}
	tool := NewReplaceFileTool(ws, NewFileCache(), app)

	args := mustMarshal(t, replaceArgs{
		Path:    "src/main.lua",
		Content: newContent,
		Reason:  "swap placeholder for real implementation",
	})
	call := llm.ToolCall{
		ID:       "rp-1",
		Function: llm.FunctionCall{Name: "replace_file", Arguments: string(args)},
	}

	tool.Execute(context.Background(), call)
	if app.received == nil {
		t.Fatalf("expected approver to receive a proposal")
	}
	if app.received.Edit.Search != oldContent {
		t.Errorf("Edit.Search = %q, want existing content", app.received.Edit.Search)
	}
	if app.received.Edit.Replace != newContent {
		t.Errorf("Edit.Replace = %q, want new content", app.received.Edit.Replace)
	}
	if app.received.ExpectedContent != newContent {
		t.Errorf("ExpectedContent = %q, want new content", app.received.ExpectedContent)
	}
	if app.received.Edit.ID != call.ID {
		t.Errorf("Edit.ID = %q, want %q", app.received.Edit.ID, call.ID)
	}
}

// TestReplaceFileTool_MissingFileSteersToWriteFile verifies the
// dedicated "file does not exist" error message — single
// responsibility means replace_file errors instead of falling back
// to creation, and the error explicitly tells the LLM to use
// write_file.
func TestReplaceFileTool_MissingFileSteersToWriteFile(t *testing.T) {
	t.Parallel()

	ws := &missingFileWorkspace{
		testWorkspace: &testWorkspace{},
	}
	app := &fakeApprover{}
	tool := NewReplaceFileTool(ws, NewFileCache(), app)

	args := mustMarshal(t, replaceArgs{
		Path:    "src/new.lua",
		Content: "x",
	})
	result := tool.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{Name: "replace_file", Arguments: string(args)},
	})
	if app.received != nil {
		t.Fatalf("approver must not see a proposal when file is missing")
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
// error instead of silently submitting a no-op proposal.
func TestReplaceFileTool_NoOpRewriteFailsLoud(t *testing.T) {
	t.Parallel()

	const same = "local same = true\n"
	ws := &testWorkspace{
		files: map[string]string{"main.lua": same},
	}
	app := &fakeApprover{}
	tool := NewReplaceFileTool(ws, NewFileCache(), app)

	args := mustMarshal(t, replaceArgs{
		Path:    "main.lua",
		Content: same,
	})
	result := tool.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{Name: "replace_file", Arguments: string(args)},
	})
	if app.received != nil {
		t.Fatalf("approver must not see a proposal on no-op rewrite")
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
				root:  tc.root,
				files: map[string]string{},
			}
			app := &fakeApprover{}
			tool := NewReplaceFileTool(ws, NewFileCache(), app)
			args := mustMarshal(t, replaceArgs{Path: tc.path, Content: "x"})
			result := tool.Execute(context.Background(), llm.ToolCall{
				Function: llm.FunctionCall{Name: "replace_file", Arguments: string(args)},
			})
			if !strings.Contains(result.Content, tc.wantSubstr) {
				t.Errorf("error missing %q: %q", tc.wantSubstr, result.Content)
			}
			if app.received != nil {
				t.Errorf("approver must not see a proposal on validation failure")
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

	tool := NewReplaceFileTool(&testWorkspace{}, NewFileCache(), &fakeApprover{})
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

// TestReplaceFileTool_UsesCachePostApproval documents the
// architectural invariant replace_file relies on: the cache reflects
// post-approval content between agent turns, seeded by the
// orchestrator after each approved edit. The tool reads cache via
// FileReader — never the buffer directly.
func TestReplaceFileTool_UsesCachePostApproval(t *testing.T) {
	t.Parallel()

	const content = "old content\n"
	ws := &testWorkspace{
		files: map[string]string{"main.lua": content},
	}
	app := &fakeApprover{}
	tool := NewReplaceFileTool(ws, NewFileCache(), app)

	args := mustMarshal(t, replaceArgs{Path: "main.lua", Content: "new content"})
	tool.Execute(context.Background(), llm.ToolCall{
		Function: llm.FunctionCall{Name: "replace_file", Arguments: string(args)},
	})
	if app.received == nil {
		t.Fatalf("expected approver to receive a proposal")
	}
	if app.received.Edit.Search != content {
		t.Errorf("Search = %q, want %q (cache+disk path)", app.received.Edit.Search, content)
	}
}

// TestReplaceFileTool_PendingEditCarriesReason verifies the
// developer-facing reason flows through to the diff overlay header.
func TestReplaceFileTool_PendingEditCarriesReason(t *testing.T) {
	t.Parallel()

	ws := &testWorkspace{
		files: map[string]string{"x.lua": "old"},
	}
	app := &fakeApprover{}
	tool := NewReplaceFileTool(ws, NewFileCache(), app)

	const reason = "swap placeholder ghost AI for arcade-accurate behavior"
	args := mustMarshal(t, replaceArgs{Path: "x.lua", Content: "new", Reason: reason})
	tool.Execute(context.Background(), llm.ToolCall{
		ID:       "rp",
		Function: llm.FunctionCall{Name: "replace_file", Arguments: string(args)},
	})
	if app.received == nil {
		t.Fatalf("expected approver to receive a proposal")
	}
	if app.received.Edit.Reason != reason {
		t.Errorf("Edit.Reason = %q, want %q", app.received.Edit.Reason, reason)
	}
}
