package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
)

// Work-tree filesystem-collision guard. The authoritative /project.md
// lives in demarkus, not on disk; a literal project.md at the project
// root would shadow the demarkus copy and silently diverge from the
// gate's view. These tests pin the rejection contract across every
// mutation primitive (write_file, edit_file, replace_file, apply_patch)
// plus the helper itself.

func TestCollidesWithWorkTree_RootProjectMd(t *testing.T) {
	ws := &testWorkspace{root: "/tmp/proj"}
	if !collidesWithWorkTree(ws, "/tmp/proj/project.md") {
		t.Errorf("expected collision for <root>/project.md")
	}
}

func TestCollidesWithWorkTree_CaseInsensitiveBasename(t *testing.T) {
	ws := &testWorkspace{root: "/tmp/proj"}
	for _, name := range []string{"Project.MD", "PROJECT.md", "project.MD"} {
		if !collidesWithWorkTree(ws, "/tmp/proj/"+name) {
			t.Errorf("expected collision for %s on case-preserving filesystem", name)
		}
	}
}

func TestCollidesWithWorkTree_SubdirectoryAllowed(t *testing.T) {
	// Nested project.md files (e.g. docs/project.md) are ordinary
	// project files, not the work tree.
	ws := &testWorkspace{root: "/tmp/proj"}
	for _, path := range []string{
		"/tmp/proj/docs/project.md",
		"/tmp/proj/kit/memory/project.md",
		"/tmp/proj/sub/dir/project.md",
	} {
		if collidesWithWorkTree(ws, path) {
			t.Errorf("nested %s must not be flagged as work-tree collision", path)
		}
	}
}

func TestCollidesWithWorkTree_DifferentBasename(t *testing.T) {
	ws := &testWorkspace{root: "/tmp/proj"}
	for _, path := range []string{
		"/tmp/proj/README.md",
		"/tmp/proj/projects.md",
		"/tmp/proj/project.txt",
	} {
		if collidesWithWorkTree(ws, path) {
			t.Errorf("%s must not be flagged", path)
		}
	}
}

func TestCollidesWithWorkTree_NoRootDisablesCheck(t *testing.T) {
	// Test workspaces without a configured root (canonpath is identity)
	// should not gate — matches the inProject "no root configured"
	// carve-out.
	ws := &testWorkspace{}
	if collidesWithWorkTree(ws, "project.md") {
		t.Errorf("guard must be off when ProjectRoot is empty")
	}
}

// --- write_file ---

func TestWriteFileTool_RejectsRootProjectMd(t *testing.T) {
	ws := &testWorkspace{root: "/tmp/proj"}
	tool := NewWriteFileTool(ws, NewFileCache(), fakeFileCreator{})
	args := mustMarshal(t, writeArgs{Path: "project.md", Content: "# Stale", Reason: "bootstrap"})
	res := tool.Execute(context.Background(), llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "write_file", Arguments: string(args)},
	})
	if !res.IsError {
		t.Fatalf("expected error result, got: %q", res.Content)
	}
	if !strings.Contains(res.Content, "work tree") {
		t.Errorf("expected work-tree error message, got: %q", res.Content)
	}
	if !strings.Contains(res.Content, "project_init") {
		t.Errorf("expected guidance toward project_init, got: %q", res.Content)
	}
}

func TestWriteFileTool_AllowsNestedProjectMd(t *testing.T) {
	ws := &testWorkspace{root: "/tmp/proj"}
	tool := NewWriteFileTool(ws, NewFileCache(), fakeFileCreator{})
	args := mustMarshal(t, writeArgs{Path: "docs/project.md", Content: "# Nested", Reason: "nested doc"})
	res := tool.Execute(context.Background(), llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "write_file", Arguments: string(args)},
	})
	if res.IsError {
		t.Errorf("nested project.md should pass: %q", res.Content)
	}
}

// --- replace_file ---

func TestReplaceFileTool_RejectsRootProjectMd(t *testing.T) {
	ws := &testWorkspace{root: "/tmp/proj", files: map[string]string{"project.md": "# old\n"}}
	tool := NewReplaceFileTool(ws, NewFileCache(), &fakeApprover{})
	args := mustMarshal(t, replaceArgs{Path: "project.md", Content: "# new\n", Reason: "rewrite"})
	res := tool.Execute(context.Background(), llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "replace_file", Arguments: string(args)},
	})
	if !res.IsError {
		t.Fatalf("expected error, got: %q", res.Content)
	}
	if !strings.Contains(res.Content, "work tree") {
		t.Errorf("expected work-tree error, got: %q", res.Content)
	}
}

// --- edit_file ---

func TestEditFileTool_RejectsRootProjectMd(t *testing.T) {
	ws := &testWorkspace{root: "/tmp/proj", files: map[string]string{"project.md": "# title\nbody\n"}}
	tool := NewEditFileTool(ws, NewFileCache(), &fakeApprover{})
	args := mustMarshal(t, editArgs{
		Path: "project.md", Search: "body", Replace: "rewritten", Reason: "patch",
	})
	res := tool.Execute(context.Background(), llm.ToolCall{
		ID:       "1",
		Function: llm.FunctionCall{Name: "edit_file", Arguments: string(args)},
	})
	if !res.IsError {
		t.Fatalf("expected error, got: %q", res.Content)
	}
	if !strings.Contains(res.Content, "work tree") {
		t.Errorf("expected work-tree error, got: %q", res.Content)
	}
}

// --- apply_patch ---

func TestApplyPatchTool_RejectsRootProjectMd(t *testing.T) {
	ws := &testWorkspace{root: "/tmp/proj", files: map[string]string{"project.md": "alpha\nbeta\n"}}
	tool := NewApplyPatchTool(ws, NewFileCache(), &fakeApprover{})
	patchText := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: project.md",
		"@@",
		"-alpha",
		"+ALPHA",
		"*** End Patch",
	}, "\n")
	res := applyPatchCall(t, tool, patchText, "patch")
	if !res.IsError {
		t.Fatalf("expected error, got: %q", res.Content)
	}
	if !strings.Contains(res.Content, "work tree") {
		t.Errorf("expected work-tree error, got: %q", res.Content)
	}
}

// fakeFileCreator is the minimal collaborator the write_file tool
// expects for FileCreated callbacks; the guard tests reject before
// any creation fires, so this records nothing.
type fakeFileCreator struct{}

func (fakeFileCreator) FileCreated(_ context.Context, _ string) {}
