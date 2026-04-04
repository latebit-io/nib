package headless

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestDiskWorkspace_ProjectRoot(t *testing.T) {
	dir := t.TempDir()
	ws := NewDiskWorkspace(dir)
	if got := ws.ProjectRoot(); got != dir {
		t.Errorf("ProjectRoot() = %q, want %q", got, dir)
	}
}

func TestDiskWorkspace_ReadFile(t *testing.T) {
	dir := t.TempDir()
	ws := NewDiskWorkspace(dir)

	tests := []struct {
		name    string
		path    string
		content string
		wantErr string
	}{
		{
			name:    "relative path",
			path:    "main.go",
			content: "package main",
		},
		{
			name:    "nested path",
			path:    "internal/foo.go",
			content: "package internal\n\nfunc Foo() {}",
		},
		{
			name:    "trailing newline trimmed",
			path:    "trimmed.go",
			content: "package trimmed\n",
		},
		{
			name:    "nonexistent file",
			path:    "nope.go",
			wantErr: "read nope.go",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wantErr == "" {
				writeTestFile(t, dir, tt.path, tt.content)
			}
			got, err := ws.ReadFile(tt.path)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			want := strings.TrimSuffix(tt.content, "\n")
			if got != want {
				t.Errorf("ReadFile(%q) = %q, want %q", tt.path, got, want)
			}
		})
	}
}

func TestDiskWorkspace_ReadFile_AbsolutePath(t *testing.T) {
	dir := t.TempDir()
	ws := NewDiskWorkspace(dir)

	writeTestFile(t, dir, "abs.go", "package abs")
	absPath := filepath.Join(dir, "abs.go")

	got, err := ws.ReadFile(absPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "package abs" {
		t.Errorf("ReadFile(%q) = %q, want %q", absPath, got, "package abs")
	}
}

func TestDiskWorkspace_WriteFile(t *testing.T) {
	dir := t.TempDir()
	ws := NewDiskWorkspace(dir)

	tests := []struct {
		name    string
		path    string
		content string
		wantErr string
	}{
		{
			name:    "simple file",
			path:    "new.go",
			content: "package new",
		},
		{
			name:    "nested creates dirs",
			path:    "deep/nested/file.go",
			content: "package deep",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ws.WriteFile(tt.path, tt.content)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Verify the file was written.
			abs := filepath.Join(dir, tt.path)
			data, err := os.ReadFile(abs)
			if err != nil {
				t.Fatalf("file not on disk: %v", err)
			}
			if string(data) != tt.content {
				t.Errorf("file content = %q, want %q", string(data), tt.content)
			}
		})
	}
}

func TestDiskWorkspace_WriteFile_AlreadyExists(t *testing.T) {
	dir := t.TempDir()
	ws := NewDiskWorkspace(dir)

	writeTestFile(t, dir, "exists.go", "original")

	err := ws.WriteFile("exists.go", "overwrite attempt")
	if err == nil {
		t.Fatal("expected error for existing file, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error %q does not mention 'already exists'", err)
	}
}

func TestDiskWorkspace_CanonPath(t *testing.T) {
	dir := t.TempDir()
	ws := NewDiskWorkspace(dir)

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "relative",
			path: "main.go",
			want: filepath.Join(dir, "main.go"),
		},
		{
			name: "absolute",
			path: filepath.Join(dir, "main.go"),
			want: filepath.Join(dir, "main.go"),
		},
		{
			name: "with dots",
			path: "internal/../main.go",
			want: filepath.Join(dir, "main.go"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ws.CanonPath(tt.path)
			if got != tt.want {
				t.Errorf("CanonPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestDiskWorkspace_InContext_AlwaysTrue(t *testing.T) {
	ws := NewDiskWorkspace(t.TempDir())
	if !ws.InContext("anything.go") {
		t.Error("InContext should always return true in headless mode")
	}
}

func TestDiskWorkspace_AddContext_TracksFiles(t *testing.T) {
	dir := t.TempDir()
	ws := NewDiskWorkspace(dir)

	ws.AddContext("a.go")
	ws.AddContext("b.go")
	ws.AddContext("a.go") // duplicate

	touched := ws.TouchedFiles()
	sort.Strings(touched)

	if len(touched) != 2 {
		t.Fatalf("TouchedFiles() returned %d files, want 2", len(touched))
	}
	wantA := filepath.Join(dir, "a.go")
	wantB := filepath.Join(dir, "b.go")
	if touched[0] != wantA || touched[1] != wantB {
		t.Errorf("TouchedFiles() = %v, want [%s, %s]", touched, wantA, wantB)
	}
}

func TestDiskWorkspace_WriteFile_TracksContext(t *testing.T) {
	dir := t.TempDir()
	ws := NewDiskWorkspace(dir)

	if err := ws.WriteFile("created.go", "package created"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	touched := ws.TouchedFiles()
	if len(touched) != 1 {
		t.Fatalf("expected 1 touched file, got %d", len(touched))
	}
	want := filepath.Join(dir, "created.go")
	if touched[0] != want {
		t.Errorf("TouchedFiles()[0] = %q, want %q", touched[0], want)
	}
}

func TestDiskWorkspace_ResolvePath_Traversal(t *testing.T) {
	// Create two sibling dirs so traversal from one to the other is detectable.
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	sibling := filepath.Join(parent, "secret")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, sibling, "passwd", "secret data")

	ws := NewDiskWorkspace(root)

	t.Run("ReadFile", func(t *testing.T) {
		_, err := ws.ReadFile("../secret/passwd")
		if err == nil {
			t.Fatal("expected error for path traversal, got nil")
		}
		if !strings.Contains(err.Error(), "outside project root") {
			t.Errorf("error %q does not mention 'outside project root'", err)
		}
	})

	t.Run("WriteFile", func(t *testing.T) {
		err := ws.WriteFile("../secret/new.txt", "injected")
		if err == nil {
			t.Fatal("expected error for path traversal, got nil")
		}
		if !strings.Contains(err.Error(), "outside project root") {
			t.Errorf("error %q does not mention 'outside project root'", err)
		}
	})

	t.Run("OverwriteFile", func(t *testing.T) {
		err := ws.OverwriteFile("../secret/passwd", "overwritten")
		if err == nil {
			t.Fatal("expected error for path traversal, got nil")
		}
		if !strings.Contains(err.Error(), "outside project root") {
			t.Errorf("error %q does not mention 'outside project root'", err)
		}
	})

	t.Run("symlink escape", func(t *testing.T) {
		// A symlink inside root that points outside should be caught.
		link := filepath.Join(root, "link")
		target := filepath.Join(sibling, "passwd")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks not supported: %v", err)
		}

		_, err := ws.ReadFile("link")
		if err == nil {
			t.Fatal("expected error for symlink escape, got nil")
		}
		if !strings.Contains(err.Error(), "outside project root") {
			t.Errorf("error %q does not mention 'outside project root'", err)
		}
	})
}

func TestDiskWorkspace_ListFiles(t *testing.T) {
	dir := t.TempDir()
	ws := NewDiskWorkspace(dir)

	writeTestFile(t, dir, "a.go", "package a")
	writeTestFile(t, dir, "sub/b.go", "package b")

	files, err := ws.ListFiles()
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}

	sort.Strings(files)
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(files), files)
	}
	if files[0] != "a.go" || files[1] != "sub/b.go" {
		t.Errorf("ListFiles() = %v, want [a.go, sub/b.go]", files)
	}
}

// writeTestFile creates a file under dir, creating parent directories as needed.
func writeTestFile(t *testing.T, dir, relPath, content string) {
	t.Helper()
	abs := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}
