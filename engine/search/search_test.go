package search

import (
	"os"
	"path/filepath"
	"testing"
)

func setupTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// Create test files.
	writeFile(t, dir, "main.go", "package main\n\nfunc main() {\n\tfmt.Println(\"hello world\")\n}\n")
	writeFile(t, dir, "util.go", "package main\n\nfunc helper() string {\n\treturn \"hello\"\n}\n")
	writeFile(t, dir, "README.md", "# Project\n\nThis is a test project.\n")
	writeFile(t, dir, "sub/nested.go", "package sub\n\n// TODO: implement\nfunc Nested() {}\n")

	return dir
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSearch_GoNative_Literal(t *testing.T) {
	dir := setupTestDir(t)

	results, err := searchGoNative(dir, "hello", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < 2 {
		t.Fatalf("expected at least 2 matches, got %d", len(results))
	}
	for _, r := range results {
		if r.Line == 0 {
			t.Errorf("line number should be 1-indexed, got 0 for %s", r.Path)
		}
	}
}

func TestSearch_GoNative_CaseSensitive(t *testing.T) {
	dir := setupTestDir(t)

	results, err := searchGoNative(dir, "Hello", Options{CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	// "Hello" with capital H should not match "hello" in code.
	for _, r := range results {
		if r.Path == "main.go" || r.Path == "util.go" {
			t.Errorf("case-sensitive search matched %s unexpectedly", r.Path)
		}
	}
}

func TestSearch_GoNative_Regex(t *testing.T) {
	dir := setupTestDir(t)

	results, err := searchGoNative(dir, `func \w+\(`, Options{Regex: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < 3 {
		t.Fatalf("expected at least 3 func matches, got %d", len(results))
	}
}

func TestSearch_GoNative_FileGlob(t *testing.T) {
	dir := setupTestDir(t)

	results, err := searchGoNative(dir, "hello", Options{FileGlob: "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if filepath.Ext(r.Path) != ".go" {
			t.Errorf("expected only .go files, got %s", r.Path)
		}
	}
}

func TestSearch_GoNative_MaxResults(t *testing.T) {
	dir := setupTestDir(t)

	results, err := searchGoNative(dir, "a", Options{MaxResults: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) > 2 {
		t.Errorf("expected at most 2 results, got %d", len(results))
	}
}

func TestSearch_GoNative_NoMatch(t *testing.T) {
	dir := setupTestDir(t)

	results, err := searchGoNative(dir, "zzz_nonexistent_zzz", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

func TestSearch_EmptyPattern(t *testing.T) {
	dir := setupTestDir(t)
	results, err := Search(dir, "", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if results != nil {
		t.Errorf("expected nil for empty pattern, got %v", results)
	}
}
