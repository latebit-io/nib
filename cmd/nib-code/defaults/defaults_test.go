package defaults

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSeed_ColdStartCreatesFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "commands")
	if err := Seed(dir); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	for _, name := range CommandFiles() {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected %s to exist; got %v", path, err)
		}
	}
}

func TestSeed_ExistingDirIsLeftUntouched(t *testing.T) {
	// Once .project/commands/ exists, Seed must NOT overwrite or
	// add anything — even an empty dir is the user's signal of
	// "I own this now."
	dir := filepath.Join(t.TempDir(), "commands")
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Write a sentinel file so we can detect any modification.
	sentinel := filepath.Join(dir, "user.md")
	if err := os.WriteFile(sentinel, []byte("user content"), 0600); err != nil {
		t.Fatalf("sentinel: %v", err)
	}
	if err := Seed(dir); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	got, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if string(got) != "user content" {
		t.Errorf("Seed should have left existing dir alone; sentinel = %q", got)
	}
	// And no embedded defaults should have been added.
	for _, name := range CommandFiles() {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			t.Errorf("Seed wrote default %s into a pre-existing dir", path)
		}
	}
}

func TestSeed_ContentsMatchEmbed(t *testing.T) {
	// Locks the single-source-of-truth invariant: a seeded file's
	// bytes are identical to what's in the embed FS. A future
	// "templating during seed" change would have to consciously
	// break this test.
	dir := filepath.Join(t.TempDir(), "commands")
	if err := Seed(dir); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	for _, name := range CommandFiles() {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		want, err := commandsFS.ReadFile("commands/" + name)
		if err != nil {
			t.Fatalf("read embed %s: %v", name, err)
		}
		if string(got) != string(want) {
			t.Errorf("seeded %s differs from embed", name)
		}
	}
}

func TestSeed_ParentsCreated(t *testing.T) {
	// Seed should create any missing parent directories — the
	// caller may pass <projectRoot>/.project/commands where neither
	// .project nor commands exists yet.
	dir := filepath.Join(t.TempDir(), "deeply", "nested", "commands")
	if err := Seed(dir); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	for _, name := range CommandFiles() {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s under nested path; got %v", name, err)
		}
	}
}

func TestCommandFiles_NotEmpty(t *testing.T) {
	// Sanity: the embed actually bundles something. A misconfigured
	// //go:embed directive (e.g. no .md files matched) would silently
	// produce an empty FS; this test catches that.
	files := CommandFiles()
	if len(files) == 0 {
		t.Errorf("CommandFiles is empty — embed directive likely broken")
	}
	for _, f := range files {
		if !strings.HasSuffix(f, ".md") {
			t.Errorf("CommandFiles entry %q is not a .md file", f)
		}
	}
}
