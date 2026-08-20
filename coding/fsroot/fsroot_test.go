package fsroot

import (
	"errors"
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

// TestResolve_FilesystemRoot: a root of "/" must accept every absolute
// path (a prefix test against "/"+Separator would reject them all).
func TestResolve_FilesystemRoot(t *testing.T) {
	r := New(string(filepath.Separator))
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "f.txt"), "x\n")
	if _, err := r.Resolve(filepath.Join(dir, "f.txt")); err != nil {
		t.Fatalf("Resolve under / root: %v", err)
	}
	if got, err := r.ReadFile(filepath.Join(dir, "f.txt")); err != nil || got != "x" {
		t.Fatalf("ReadFile under / root = %q, %v", got, err)
	}
}

// TestIO_RealPathThroughSymlinkedRoot: when the root itself is reached
// through a symlink (macOS temp dirs), a caller passing the resolved
// real path still addresses the same file.
func TestIO_RealPathThroughSymlinkedRoot(t *testing.T) {
	parent := t.TempDir()
	realRoot := filepath.Join(parent, "real")
	linkRoot := filepath.Join(parent, "link")
	if err := os.Mkdir(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	r := New(linkRoot)
	if _, err := r.WriteFile(filepath.Join(realRoot, "real.go"), "package real\n"); err != nil {
		t.Fatal(err)
	}
	if got, err := r.ReadFile("real.go"); err != nil || got != "package real" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
}

// TestReadFile_TooLarge: reads are bounded by MaxFileSize so a huge
// workspace file cannot balloon memory or the prompt.
func TestReadFile_TooLarge(t *testing.T) {
	r, _ := setup(t)
	big := filepath.Join(r.Dir(), "big.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse file: cheap to create, Stat reports the full size.
	if err := f.Truncate(MaxFileSize + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadFile("big.bin"); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("ReadFile(big) = %v, want ErrFileTooLarge", err)
	}
	mustWrite(t, filepath.Join(r.Dir(), "small.txt"), "ok\n")
	if got, err := r.ReadFile("small.txt"); err != nil || got != "ok" {
		t.Fatalf("ReadFile(small) = %q, %v", got, err)
	}
}
