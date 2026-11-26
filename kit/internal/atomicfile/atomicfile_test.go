package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWrite_ReplacesAndLeavesNoTemp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	for _, body := range []string{"first", "second"} {
		if err := Write(path, []byte(body), 0o700, 0o600); err != nil {
			t.Fatalf("Write(%q): %v", body, err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != body {
			t.Fatalf("file = %q, %v; want %q", got, err, body)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Errorf("directory holds %d entries, %v; want only the file", len(entries), err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
}
