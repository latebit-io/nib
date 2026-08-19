package fsroot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setup returns a root and a sibling "secret" dir outside it.
func setup(t *testing.T) (Root, string) {
	t.Helper()
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	secret := filepath.Join(parent, "secret")
	for _, d := range []string{root, secret} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return New(root), secret
}

func TestResolve_InsideRoot(t *testing.T) {
	r, _ := setup(t)
	mustWrite(t, filepath.Join(r.Dir(), "src", "main.go"), "x")
	for _, p := range []string{"src/main.go", filepath.Join(r.Dir(), "src/main.go"), "src/../src/main.go", "new/deep/file.go"} {
		if _, err := r.Resolve(p); err != nil {
			t.Errorf("Resolve(%q): %v", p, err)
		}
	}
}

func TestResolve_Traversal(t *testing.T) {
	r, secret := setup(t)
	mustWrite(t, filepath.Join(secret, "passwd"), "s")
	for _, p := range []string{"../secret/passwd", "/etc/passwd", "../secret/new.txt", "../secret/newdir/x.txt"} {
		if _, err := r.Resolve(p); err == nil {
			t.Errorf("Resolve(%q): want error, got nil", p)
		}
	}
}

// TestResolve_SymlinkEscape_MissingTail is the regression for the
// session workspace, which returned abs when neither target nor parent
// existed — letting `link/newdir/f.go` through an in-root symlink escape.
func TestResolve_SymlinkEscape_MissingTail(t *testing.T) {
	r, secret := setup(t)
	link := filepath.Join(r.Dir(), "link")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	cases := []string{"link/newdir/f.go", "link/f.go", "link/a/b/c/d.go"}
	for _, p := range cases {
		if _, err := r.Resolve(p); err == nil || !strings.Contains(err.Error(), "outside project root") {
			t.Errorf("Resolve(%q) = %v, want outside-root error", p, err)
		}
		if _, err := r.WriteFile(p, "x"); err == nil {
			t.Errorf("WriteFile(%q) succeeded, want rejection", p)
		}
	}
	if entries, _ := os.ReadDir(secret); len(entries) != 0 {
		t.Errorf("secret dir polluted: %v", entries)
	}
}

func TestResolve_LeafSymlinkEscape(t *testing.T) {
	r, secret := setup(t)
	mustWrite(t, filepath.Join(secret, "passwd"), "s")
	if err := os.Symlink(filepath.Join(secret, "passwd"), filepath.Join(r.Dir(), "link")); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	if _, err := r.ReadFile("link"); err == nil {
		t.Fatal("ReadFile through escaping symlink succeeded")
	}
}

func TestWriteFile_ExclAndRead(t *testing.T) {
	r, _ := setup(t)
	abs, err := r.WriteFile("deep/nested/a.go", "package a\n")
	if err != nil {
		t.Fatal(err)
	}
	if abs != filepath.Join(r.Dir(), "deep/nested/a.go") {
		t.Errorf("abs = %q", abs)
	}
	if _, err := r.WriteFile("deep/nested/a.go", "again"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second WriteFile = %v, want already-exists", err)
	}
	got, err := r.ReadFile("deep/nested/a.go")
	if err != nil || got != "package a" {
		t.Errorf("ReadFile = %q, %v", got, err)
	}
	if err := r.OverwriteFile("deep/nested/a.go", "new"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.ReadFile("deep/nested/a.go"); got != "new" {
		t.Errorf("after overwrite = %q", got)
	}
	if err := r.MkdirAll("mk/dir"); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(r.Dir(), "mk/dir")); err != nil || !st.IsDir() {
		t.Errorf("MkdirAll: %v", err)
	}
}

func TestCanonPath(t *testing.T) {
	r := New("/proj")
	if got := r.CanonPath("a/../b.go"); got != "/proj/b.go" {
		t.Errorf("CanonPath = %q", got)
	}
	if got := r.CanonPath("/x/y.go"); got != "/x/y.go" {
		t.Errorf("CanonPath abs = %q", got)
	}
}
