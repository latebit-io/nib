package cmdallow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// writeAllowlist seeds a .project/bash-allow.json under root.
func writeAllowlist(t *testing.T, root string, rules ...string) {
	t.Helper()
	dir := filepath.Join(root, ".project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(fileFormat{Allow: rules})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	l := Load(t.TempDir())
	if l.Permits("go test ./...") {
		t.Fatal("empty allowlist permitted a command")
	}
	if l.Rules() != nil {
		t.Fatalf("Rules() = %v, want nil", l.Rules())
	}
}

func TestLoadInvalidJSONFailsClosed(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := Load(root)
	if l.Permits("ls") {
		t.Fatal("broken allowlist file must fail closed (permit nothing)")
	}
}

func TestLoadSkipsInvalidRuleKeepsRest(t *testing.T) {
	root := t.TempDir()
	writeAllowlist(t, root, "Bash(", "Bash(git status)")
	l := Load(root)
	if !l.Permits("git status") {
		t.Fatal("valid rule lost when a sibling rule was invalid")
	}
	if got := len(l.Rules()); got != 1 {
		t.Fatalf("Rules() has %d entries, want 1 (invalid one skipped)", got)
	}
}

func TestPermits(t *testing.T) {
	root := t.TempDir()
	writeAllowlist(t, root, "Bash(git status)", "Bash(go test *)")
	l := Load(root)

	cases := []struct {
		name string
		cmd  string
		want bool
	}{
		{"exact rule matches", "git status", true},
		{"exact rule does not widen", "git status --porcelain", false},
		{"glob rule matches", "go test ./...", true},
		{"unlisted denied", "git push", false},
		{"compound denied despite glob match", "go test ./... && git push", false},
		{"substitution denied", "go test $(rm -rf /)", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := l.Permits(c.cmd); got != c.want {
				t.Fatalf("Permits(%q) = %v, want %v", c.cmd, got, c.want)
			}
		})
	}
}

func TestPermitsBareBashRuleIsYolo(t *testing.T) {
	root := t.TempDir()
	writeAllowlist(t, root, "Bash")
	l := Load(root)
	for _, cmd := range []string{"anything", "git push --force", "a; b | c"} {
		if !l.Permits(cmd) {
			t.Fatalf("bare Bash rule must permit %q (explicit yolo opt-out)", cmd)
		}
	}
}

func TestPermitsExactStoredCompound(t *testing.T) {
	l := Load(t.TempDir())
	if err := l.Add("go build ./... && go test ./..."); err != nil {
		t.Fatal(err)
	}
	if !l.Permits("go build ./... && go test ./...") {
		t.Fatal("byte-identical compound must be permitted after Add")
	}
	if l.Permits("go build ./... && rm -rf /") {
		t.Fatal("different compound permitted by an exact compound rule")
	}
}

func TestNilListIsSafe(t *testing.T) {
	var l *List
	if l.Permits("ls") {
		t.Fatal("nil list permitted a command")
	}
	if l.Rules() != nil {
		t.Fatal("nil list returned rules")
	}
	if err := l.Add("ls"); err == nil {
		t.Fatal("Add on nil list must error")
	}
}

func TestAddPersistsAndReloads(t *testing.T) {
	root := t.TempDir()
	l := Load(root)
	if err := l.Add("go test ./..."); err != nil {
		t.Fatal(err)
	}
	if !l.Permits("go test ./...") {
		t.Fatal("added rule not live in-memory")
	}
	// A fresh Load must see the persisted rule.
	if !Load(root).Permits("go test ./...") {
		t.Fatal("added rule did not survive reload")
	}
}

func TestAddDeduplicates(t *testing.T) {
	root := t.TempDir()
	l := Load(root)
	for range 3 {
		if err := l.Add("git status"); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(l.Rules()); got != 1 {
		t.Fatalf("Rules() has %d entries after duplicate Adds, want 1", got)
	}
}

func TestAddRollsBackOnPersistFailure(t *testing.T) {
	root := t.TempDir()
	// Occupy the .project path with a FILE so MkdirAll fails.
	if err := os.WriteFile(filepath.Join(root, ".project"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := Load(root)
	if err := l.Add("git status"); err == nil {
		t.Fatal("Add must surface the persist failure")
	}
	if l.Permits("git status") {
		t.Fatal("failed Add left the rule live in memory — memory and disk diverge on restart")
	}
}

func TestConcurrentPermitsAndAdd(t *testing.T) {
	l := Load(t.TempDir())
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			// Errors irrelevant here — the test is the race detector.
			_ = l.Add("cmd " + string(rune('a'+i)))
		}()
		go func() {
			defer wg.Done()
			l.Permits("cmd a")
		}()
	}
	wg.Wait()
}
