package filelist

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWalk_BasicFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), "package main")
	writeFile(t, filepath.Join(dir, "lib", "util.go"), "package lib")

	files, err := Walk(dir)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"lib/util.go", "main.go"}
	if len(files) != len(want) {
		t.Fatalf("got %d files, want %d: %v", len(files), len(want), files)
	}
	for i, f := range files {
		if f != want[i] {
			t.Errorf("files[%d] = %q, want %q", i, f, want[i])
		}
	}
}

func TestWalk_SkipsGitDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), "package main")
	writeFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main")
	writeFile(t, filepath.Join(dir, ".git", "config"), "[core]")

	files, err := Walk(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 1 || files[0] != "main.go" {
		t.Errorf("got %v, want [main.go]", files)
	}
}

func TestWalk_RespectsGitignore(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".gitignore"), "*.log\nbuild/\n")
	writeFile(t, filepath.Join(dir, "main.go"), "package main")
	writeFile(t, filepath.Join(dir, "error.log"), "log content")
	writeFile(t, filepath.Join(dir, "build", "output.exe"), "binary")

	files, err := Walk(dir)
	if err != nil {
		t.Fatal(err)
	}

	// .gitignore itself should be listed; *.log and build/ should be excluded.
	want := []string{".gitignore", "main.go"}
	if len(files) != len(want) {
		t.Fatalf("got %v, want %v", files, want)
	}
	for i, f := range files {
		if f != want[i] {
			t.Errorf("files[%d] = %q, want %q", i, f, want[i])
		}
	}
}

func TestWalk_NegationPattern(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".gitignore"), "*.log\n!important.log\n")
	writeFile(t, filepath.Join(dir, "error.log"), "excluded")
	writeFile(t, filepath.Join(dir, "important.log"), "included")

	files, err := Walk(dir)
	if err != nil {
		t.Fatal(err)
	}

	// important.log should be included (negation), error.log excluded.
	found := false
	for _, f := range files {
		if f == "error.log" {
			t.Error("error.log should be excluded")
		}
		if f == "important.log" {
			found = true
		}
	}
	if !found {
		t.Error("important.log should be included via negation")
	}
}

func TestWalk_NestedGitignore(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".gitignore"), "*.tmp\n")
	writeFile(t, filepath.Join(dir, "sub", ".gitignore"), "*.bak\n")
	writeFile(t, filepath.Join(dir, "root.tmp"), "excluded by root")
	writeFile(t, filepath.Join(dir, "sub", "file.bak"), "excluded by sub")
	writeFile(t, filepath.Join(dir, "sub", "file.go"), "included")
	writeFile(t, filepath.Join(dir, "main.go"), "included")

	files, err := Walk(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range files {
		if f == "root.tmp" || f == "sub/file.bak" {
			t.Errorf("file %q should be excluded", f)
		}
	}
	// Check that included files are present.
	wantFiles := map[string]bool{"main.go": false, "sub/file.go": false}
	for _, f := range files {
		if _, ok := wantFiles[f]; ok {
			wantFiles[f] = true
		}
	}
	for f, found := range wantFiles {
		if !found {
			t.Errorf("expected file %q not found", f)
		}
	}
}

func TestWalk_DoubleStarPattern(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".gitignore"), "**/*.tmp\n")
	writeFile(t, filepath.Join(dir, "root.tmp"), "excluded")
	writeFile(t, filepath.Join(dir, "a", "b", "deep.tmp"), "excluded")
	writeFile(t, filepath.Join(dir, "main.go"), "included")

	files, err := Walk(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range files {
		if f == "root.tmp" || f == "a/b/deep.tmp" {
			t.Errorf("file %q should be excluded by **/*.tmp", f)
		}
	}
}

func TestWalk_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	files, err := Walk(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("empty dir should return 0 files, got %d", len(files))
	}
}

func TestWalk_SortedOutput(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "c.go"), "")
	writeFile(t, filepath.Join(dir, "a.go"), "")
	writeFile(t, filepath.Join(dir, "b.go"), "")

	files, err := Walk(dir)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"a.go", "b.go", "c.go"}
	if len(files) != len(want) {
		t.Fatalf("got %v, want %v", files, want)
	}
	for i, f := range files {
		if f != want[i] {
			t.Errorf("files[%d] = %q, want %q", i, f, want[i])
		}
	}
}
