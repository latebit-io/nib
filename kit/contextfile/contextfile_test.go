package contextfile

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadPrefersAgentsMD(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "AGENTS.md", "agents body")
	write(t, dir, "CLAUDE.md", "claude body")

	files, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	if files[0].Content != "agents body" {
		t.Errorf("want AGENTS.md to win, got content %q", files[0].Content)
	}
	if !strings.HasSuffix(files[0].Path, "AGENTS.md") {
		t.Errorf("Path = %q, want .../AGENTS.md", files[0].Path)
	}
	if !filepath.IsAbs(files[0].Path) {
		t.Errorf("Path = %q, want absolute", files[0].Path)
	}
}

func TestLoadFallsBackToClaudeMD(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "CLAUDE.md", "claude body")

	files, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(files) != 1 || files[0].Content != "claude body" {
		t.Fatalf("want CLAUDE.md fallback, got %v", files)
	}
}

func TestLoadNoneIsNilNil(t *testing.T) {
	files, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load on empty dir: %v", err)
	}
	if files != nil {
		t.Errorf("want nil slice for no context file, got %v", files)
	}
}

func TestLoadOversizeIsError(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "AGENTS.md", strings.Repeat("x", MaxBytes+1))

	files, err := Load(dir)
	if err == nil {
		t.Fatal("want error for oversize context file, got nil")
	}
	if files != nil {
		t.Errorf("oversize load must not return partial content, got %d file(s)", len(files))
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("error should name the cap, got: %v", err)
	}
}

// contents returns each file's body, in order.
func contents(files []File) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Content)
	}
	return out
}

// tree builds root/a/b and returns root and the leaf.
func tree(t *testing.T) (root, leaf string) {
	t.Helper()
	root = t.TempDir()
	leaf = filepath.Join(root, "a", "b")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}
	return root, leaf
}

// TestLoadChain_NearestLast: every file between root and the working
// directory loads, a directory without one is passed over, and the
// nearest comes last.
func TestLoadChain_NearestLast(t *testing.T) {
	root, leaf := tree(t)
	write(t, root, "AGENTS.md", "root")
	write(t, leaf, "AGENTS.md", "leaf")
	write(t, filepath.Dir(root), "AGENTS.md", "above the root")

	files, err := LoadChain(root, leaf)
	if err != nil {
		t.Fatalf("LoadChain: %v", err)
	}
	if got, want := contents(files), []string{"root", "leaf"}; !slices.Equal(got, want) {
		t.Errorf("chain = %q, want %q", got, want)
	}
}

// TestLoadChain_DirOutsideRoot: a working directory that is not under
// root contributes nothing; only root's file loads.
func TestLoadChain_DirOutsideRoot(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	write(t, root, "AGENTS.md", "root")
	write(t, other, "AGENTS.md", "other")

	files, err := LoadChain(root, other)
	if err != nil {
		t.Fatalf("LoadChain: %v", err)
	}
	if got, want := contents(files), []string{"root"}; !slices.Equal(got, want) {
		t.Errorf("chain = %q, want %q", got, want)
	}
}

// TestLoadChain_BadFileKeepsTheRest: one oversize file is reported and
// does not cost the session the other files.
func TestLoadChain_BadFileKeepsTheRest(t *testing.T) {
	root, leaf := tree(t)
	write(t, root, "AGENTS.md", strings.Repeat("x", MaxBytes+1))
	write(t, leaf, "AGENTS.md", "leaf")

	files, err := LoadChain(root, leaf)
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Errorf("err = %v, want the oversize file reported", err)
	}
	if got, want := contents(files), []string{"leaf"}; !slices.Equal(got, want) {
		t.Errorf("chain = %q, want %q", got, want)
	}
}

// TestLoadChain_TotalCapDropsFarthest: the nearest files take
// precedence, so they are the ones kept when the chain is too large.
func TestLoadChain_TotalCapDropsFarthest(t *testing.T) {
	root, leaf := tree(t)
	full := strings.Repeat("x", MaxBytes)
	write(t, root, "AGENTS.md", "root")
	write(t, filepath.Dir(leaf), "AGENTS.md", full)
	write(t, leaf, "AGENTS.md", full)

	files, err := LoadChain(root, leaf)
	if err == nil || !strings.Contains(err.Error(), "total cap") {
		t.Errorf("err = %v, want the dropped file reported", err)
	}
	if len(files) != 2 || files[0].Path != filepath.Join(filepath.Dir(leaf), "AGENTS.md") {
		t.Errorf("want the two nearest files kept, got %d: %v", len(files), files)
	}
}

// TestLoadChain_SymlinkCannotLeaveTheProject: a repository can carry
// an AGENTS.md that is a link. One pointing outside must not put that
// file's text in the prompt; one pointing inside loads.
func TestLoadChain_SymlinkCannotLeaveTheProject(t *testing.T) {
	root, leaf := tree(t)
	outside := filepath.Join(t.TempDir(), "credentials")
	write(t, filepath.Dir(outside), "credentials", "hunter2")
	if err := os.Symlink(outside, filepath.Join(leaf, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	write(t, root, "CLAUDE.md", "root rules")
	if err := os.Symlink("CLAUDE.md", filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}

	files, err := LoadChain(root, leaf)
	if err == nil {
		t.Error("the link out of the project must be reported")
	}
	if got, want := contents(files), []string{"root rules"}; !slices.Equal(got, want) {
		t.Errorf("chain = %q, want %q", got, want)
	}
}
