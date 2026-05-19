package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
)

// applyPatchCall is a small helper that wraps the JSON-call ritual so
// each test reads as a hunk-of-text → expected outcome.
func applyPatchCall(t *testing.T, tool *ApplyPatchTool, patch, reason string) ToolResult {
	t.Helper()
	args := mustMarshal(t, applyPatchArgs{Patch: patch, Reason: reason})
	return tool.Execute(context.Background(), llm.ToolCall{
		ID:       "ap-1",
		Function: llm.FunctionCall{Name: "apply_patch", Arguments: string(args)},
	})
}

// TestApplyPatchTool_SingleHunkProducesEditProposal verifies P6: a
// successful patch routes through the same [Approver] surface as
// edit_file / replace_file, with Search = pre-edit, Replace =
// post-edit, ExpectedContent = post-edit.
func TestApplyPatchTool_SingleHunkProducesEditProposal(t *testing.T) {
	t.Parallel()
	const before = "package main\n\nfunc greet() {\n\tfmt.Println(\"hi\")\n}\n"
	const after = "package main\n\nfunc greet() {\n\tfmt.Println(\"hello\")\n}\n"

	ws := &testWorkspace{files: map[string]string{"main.go": before}}
	app := &fakeApprover{}
	tool := NewApplyPatchTool(ws, NewFileCache(), app)

	patchText := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: main.go",
		"@@ func greet",
		"-\tfmt.Println(\"hi\")",
		"+\tfmt.Println(\"hello\")",
		"*** End Patch",
	}, "\n")

	res := applyPatchCall(t, tool, patchText, "say hello instead of hi")
	if res.IsError {
		t.Fatalf("unexpected error result: %q", res.Content)
	}
	if app.received == nil {
		t.Fatalf("approver did not receive a proposal")
	}
	if app.received.Edit.Search != before {
		t.Errorf("Edit.Search = %q, want pre-edit content", app.received.Edit.Search)
	}
	if app.received.Edit.Replace != after {
		t.Errorf("Edit.Replace = %q, want post-edit content", app.received.Edit.Replace)
	}
	if app.received.ExpectedContent != after {
		t.Errorf("ExpectedContent = %q, want %q", app.received.ExpectedContent, after)
	}
	if app.received.Edit.Reason != "say hello instead of hi" {
		t.Errorf("Edit.Reason = %q", app.received.Edit.Reason)
	}
}

// TestApplyPatchTool_MultipleHunksApplyInOrder verifies the
// composition story: two hunks in one envelope edit two non-adjacent
// locations in the file, and both land in the post-edit content.
func TestApplyPatchTool_MultipleHunksApplyInOrder(t *testing.T) {
	t.Parallel()
	const before = "alpha\nbeta\ngamma\ndelta\nepsilon\n"

	ws := &testWorkspace{files: map[string]string{"f.txt": before}}
	app := &fakeApprover{}
	tool := NewApplyPatchTool(ws, NewFileCache(), app)

	patchText := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: f.txt",
		"@@",
		"-alpha",
		"+ALPHA",
		"@@",
		"-delta",
		"+DELTA",
		"*** End Patch",
	}, "\n")

	res := applyPatchCall(t, tool, patchText, "uppercase two lines")
	if res.IsError {
		t.Fatalf("error result: %q", res.Content)
	}
	want := "ALPHA\nbeta\ngamma\nDELTA\nepsilon\n"
	if app.received.ExpectedContent != want {
		t.Errorf("ExpectedContent = %q, want %q", app.received.ExpectedContent, want)
	}
}

// TestApplyPatchTool_AnchorDisambiguatesRepeatedPattern verifies P3:
// when the same -/+ pair would match in more than one place, the @@
// anchor narrows the match to the intended location.
func TestApplyPatchTool_AnchorDisambiguatesRepeatedPattern(t *testing.T) {
	t.Parallel()
	const before = "func A() {\n\treturn 1\n}\n\nfunc B() {\n\treturn 1\n}\n"

	ws := &testWorkspace{files: map[string]string{"x.go": before}}
	app := &fakeApprover{}
	tool := NewApplyPatchTool(ws, NewFileCache(), app)

	patchText := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: x.go",
		"@@ func B",
		"-\treturn 1",
		"+\treturn 2",
		"*** End Patch",
	}, "\n")

	res := applyPatchCall(t, tool, patchText, "")
	if res.IsError {
		t.Fatalf("error result: %q", res.Content)
	}
	want := "func A() {\n\treturn 1\n}\n\nfunc B() {\n\treturn 2\n}\n"
	if app.received.ExpectedContent != want {
		t.Errorf("ExpectedContent =\n%q\nwant\n%q", app.received.ExpectedContent, want)
	}
}

// TestApplyPatchTool_AmbiguousMatchSurfacesActionableError verifies
// part of P4: a -/+ pair matching multiple times without an anchor
// returns a non-error-result with the LLM-facing "add an @@ anchor"
// guidance instead of guessing or applying randomly.
func TestApplyPatchTool_AmbiguousMatchSurfacesActionableError(t *testing.T) {
	t.Parallel()
	const before = "x = 1\ny = 2\nx = 1\n"

	ws := &testWorkspace{files: map[string]string{"f.txt": before}}
	app := &fakeApprover{}
	tool := NewApplyPatchTool(ws, NewFileCache(), app)

	patchText := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: f.txt",
		"-x = 1",
		"+x = 99",
		"*** End Patch",
	}, "\n")

	res := applyPatchCall(t, tool, patchText, "")
	if !res.IsError {
		t.Fatalf("want error result for ambiguous match; got %q", res.Content)
	}
	if app.received != nil {
		t.Fatalf("approver must not receive a proposal on ambiguous match")
	}
	for _, sub := range []string{"match", "anchor"} {
		if !strings.Contains(res.Content, sub) {
			t.Errorf("error missing %q: %q", sub, res.Content)
		}
	}
}

// TestApplyPatchTool_UnmatchedHunkNamesHunkNumber verifies P4: when a
// hunk fails to match, the error names the 1-indexed hunk so the LLM
// can retry just that one rather than the whole envelope.
func TestApplyPatchTool_UnmatchedHunkNamesHunkNumber(t *testing.T) {
	t.Parallel()
	const before = "alpha\nbeta\ngamma\n"

	ws := &testWorkspace{files: map[string]string{"f.txt": before}}
	app := &fakeApprover{}
	tool := NewApplyPatchTool(ws, NewFileCache(), app)

	patchText := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: f.txt",
		"@@",
		"-alpha",
		"+ALPHA",
		"@@",
		"-nonexistent",
		"+nope",
		"*** End Patch",
	}, "\n")

	res := applyPatchCall(t, tool, patchText, "")
	if !res.IsError {
		t.Fatalf("want error result; got %q", res.Content)
	}
	if !strings.Contains(res.Content, "hunk 2") {
		t.Errorf("error must name 'hunk 2' for the failing hunk; got %q", res.Content)
	}
}

// TestApplyPatchTool_MissingFileSteersToWriteFile verifies that
// patching a non-existent file returns a write_file steer, same
// shape as replace_file's missing-file message.
func TestApplyPatchTool_MissingFileSteersToWriteFile(t *testing.T) {
	t.Parallel()
	ws := &missingFileWorkspace{testWorkspace: &testWorkspace{}}
	app := &fakeApprover{}
	tool := NewApplyPatchTool(ws, NewFileCache(), app)

	patchText := "*** Begin Patch\n*** Update File: new.go\n-old\n+new\n*** End Patch"
	res := applyPatchCall(t, tool, patchText, "")
	if !res.IsError || !strings.Contains(res.Content, "write_file") {
		t.Errorf("want write_file steer; got %q", res.Content)
	}
	if app.received != nil {
		t.Errorf("approver must not see a proposal for missing file")
	}
}

// TestApplyPatchTool_ParseErrorSurfaced verifies parser-level errors
// flow back to the LLM as actionable tool errors (not silent
// failures).
func TestApplyPatchTool_ParseErrorSurfaced(t *testing.T) {
	t.Parallel()
	ws := &testWorkspace{files: map[string]string{"f.txt": "x"}}
	app := &fakeApprover{}
	tool := NewApplyPatchTool(ws, NewFileCache(), app)

	// Missing "*** End Patch"
	patchText := "*** Begin Patch\n*** Update File: f.txt\n-x\n+y"
	res := applyPatchCall(t, tool, patchText, "")
	if !res.IsError {
		t.Fatalf("want error result for malformed patch; got %q", res.Content)
	}
	if !strings.Contains(res.Content, "End Patch") {
		t.Errorf("error must mention the missing terminator; got %q", res.Content)
	}
}

// TestApplyPatchTool_AddFileSteersToWriteFile verifies the v2
// directive error carries the write_file steer back to the LLM.
func TestApplyPatchTool_AddFileSteersToWriteFile(t *testing.T) {
	t.Parallel()
	ws := &testWorkspace{}
	app := &fakeApprover{}
	tool := NewApplyPatchTool(ws, NewFileCache(), app)

	patchText := "*** Begin Patch\n*** Add File: new.go\n+package new\n*** End Patch"
	res := applyPatchCall(t, tool, patchText, "")
	if !res.IsError || !strings.Contains(res.Content, "write_file") {
		t.Errorf("want write_file steer for Add File; got %q", res.Content)
	}
}

// TestApplyPatchTool_NoChangeFailsLoud verifies an apply that
// produces identical output to the input (e.g. a hunk whose '-' and
// '+' lines coincidentally match the same content) is rejected as a
// no-op rather than submitting an empty diff to the approval flow.
func TestApplyPatchTool_NoChangeFailsLoud(t *testing.T) {
	t.Parallel()
	const before = "alpha\nbeta\n"

	ws := &testWorkspace{files: map[string]string{"f.txt": before}}
	app := &fakeApprover{}
	tool := NewApplyPatchTool(ws, NewFileCache(), app)

	patchText := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: f.txt",
		"-alpha",
		"+alpha",
		"*** End Patch",
	}, "\n")

	res := applyPatchCall(t, tool, patchText, "")
	if !res.IsError {
		t.Fatalf("want error result on no-op patch; got %q", res.Content)
	}
	if app.received != nil {
		t.Errorf("approver must not see a no-op proposal")
	}
}

// TestApplyPatchTool_OutsideProjectRootRejected verifies path
// containment matches the other file tools: a path that escapes the
// project root is rejected before any file access.
func TestApplyPatchTool_OutsideProjectRootRejected(t *testing.T) {
	t.Parallel()
	ws := &testWorkspace{root: filepath.FromSlash("/safe/project")}
	app := &fakeApprover{}
	tool := NewApplyPatchTool(ws, NewFileCache(), app)

	patchText := "*** Begin Patch\n*** Update File: /etc/passwd\n-x\n+y\n*** End Patch"
	res := applyPatchCall(t, tool, patchText, "")
	if !res.IsError || !strings.Contains(res.Content, "outside the project root") {
		t.Errorf("want path-traversal block; got %q", res.Content)
	}
}

// TestApplyPatchTool_DefinitionAdvertisesAlternatives verifies the
// tool description names write_file and replace_file so the LLM
// picks correctly between the four file-mutation primitives.
func TestApplyPatchTool_DefinitionAdvertisesAlternatives(t *testing.T) {
	t.Parallel()
	tool := NewApplyPatchTool(&testWorkspace{}, NewFileCache(), &fakeApprover{})
	def := tool.Definition()
	if def.Function.Name != "apply_patch" {
		t.Errorf("Name = %q, want apply_patch", def.Function.Name)
	}
	for _, want := range []string{"write_file", "replace_file", "edit_file", "Begin Patch", "End Patch"} {
		if !strings.Contains(def.Function.Description, want) {
			t.Errorf("description missing %q: %q", want, def.Function.Description)
		}
	}
	if len(def.Function.Parameters.Required) == 0 || def.Function.Parameters.Required[0] != "patch" {
		t.Errorf("Required = %v, want [patch]", def.Function.Parameters.Required)
	}
}
