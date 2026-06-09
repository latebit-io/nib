package lint

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
		{"LOVE main.lua", []string{"main.lua"}, true},
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

func TestDetect_LuaWithoutBinary(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.lua"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	emptyPath := t.TempDir()
	t.Setenv("PATH", emptyPath)

	if got := Detect(dir); got != nil {
		t.Errorf("expected nil when luacheck missing, got %v", got)
	}
}

func TestDetect_NonLuaNonGo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	emptyPath := t.TempDir()
	t.Setenv("PATH", emptyPath)

	if got := Detect(dir); got != nil {
		t.Errorf("expected nil for non-Lua non-Go project, got %v", got)
	}
}

func TestFromShellCommands(t *testing.T) {
	if got := FromShellCommands(nil); got != nil {
		t.Errorf("nil input should return nil, got %+v", got)
	}
	if got := FromShellCommands([]string{}); got != nil {
		t.Errorf("empty slice should return nil, got %+v", got)
	}

	cmds := []string{"echo a", "echo b"}
	got := FromShellCommands(cmds)
	if len(got) != 2 {
		t.Fatalf("expected 2 linters, got %d", len(got))
	}
	for i, l := range got {
		raw, ok := l.(*RawLinter)
		if !ok {
			t.Fatalf("linter %d is not *RawLinter: %T", i, l)
		}
		if raw.Command != cmds[i] {
			t.Errorf("linter %d command: got %q want %q", i, raw.Command, cmds[i])
		}
	}
}

func TestGoModExists(t *testing.T) {
	tmp := t.TempDir()
	if goModExists(tmp) {
		t.Error("empty dir should not report go.mod present")
	}
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !goModExists(tmp) {
		t.Error("go.mod present but goModExists returned false")
	}
}
