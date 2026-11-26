package workspace

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/nib/kit/extension"
)

// TestSpec_BuildsShellAndFileTools: the extension is on by default and
// brings the shell, the file tools, search, and its prompt text.
func TestSpec_BuildsShellAndFileTools(t *testing.T) {
	spec := Spec()
	if !spec.Default {
		t.Fatalf("spec = %+v, want it on by default", spec)
	}
	dir := t.TempDir()
	ext, err := spec.New(context.Background(), extension.Env{Dir: dir, Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range ext.Toolset.Tools {
		names = append(names, tl.Definition().Function.Name)
	}
	if got := strings.Join(names, ","); got != "bash,read_file,write_file,edit_file,glob,list_dir,search_project" {
		t.Errorf("tools = %s", got)
	}
	if !strings.Contains(ext.Prompt, dir) {
		t.Errorf("prompt does not name the project: %q", ext.Prompt)
	}
	if err := ext.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
