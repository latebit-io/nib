package contextfile

import (
	"os"
	"path/filepath"
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
