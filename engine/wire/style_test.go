package wire

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsLuaProject(t *testing.T) {
	cases := []struct {
		name    string
		files   []string
		wantLua bool
	}{
		{"empty directory", nil, false},
		{"only go files", []string{"main.go", "go.mod"}, false},
		{".luacheckrc marker", []string{".luacheckrc"}, true},
		{"LÖVE main.lua", []string{"main.lua"}, true},
		{"arbitrary .lua at root", []string{"utils.lua"}, true},
		{"mixed content", []string{"README.md", "script.lua"}, true},
		{".lua only in subdir is not detected",
			[]string{"src/game.lua"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tc.files {
				full := filepath.Join(dir, f)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(full, []byte("-- test\n"), 0o644); err != nil {
					t.Fatalf("write %s: %v", f, err)
				}
			}
			if got := isLuaProject(dir); got != tc.wantLua {
				t.Errorf("isLuaProject = %v, want %v", got, tc.wantLua)
			}
		})
	}
}

func TestDetectLintCommands_LuaWithoutBinary(t *testing.T) {
	// Force LookPath to miss by clearing PATH for this test. We can't inject
	// a stub cleanly, so we assert the "project detected, no binary" branch
	// returns nil by setting PATH to an empty directory.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.lua"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	emptyPath := t.TempDir()
	t.Setenv("PATH", emptyPath)

	if got := detectLintCommands(dir); got != nil {
		t.Errorf("expected nil when luacheck missing, got %v", got)
	}
}

func TestDetectLintCommands_NonLuaNonGo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectLintCommands(dir); got != nil {
		t.Errorf("expected nil for non-Lua non-Go project, got %v", got)
	}
}
