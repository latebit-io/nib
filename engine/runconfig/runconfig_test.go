package runconfig

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadExplicitConfig verifies an explicit smoke_command in
// .project/run.json wins over auto-detection.
func TestLoadExplicitConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".project"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"smoke_command": "echo custom-smoke", "timeout_ms": 5000}`
	if err := os.WriteFile(filepath.Join(dir, ".project", "run.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	got := Load(dir)
	if got.Skipped {
		t.Errorf("Skipped = true, want false")
	}
	if got.Command != "echo custom-smoke" {
		t.Errorf("Command = %q, want %q", got.Command, "echo custom-smoke")
	}
	if got.Source != "config" {
		t.Errorf("Source = %q, want %q", got.Source, "config")
	}
	if got.Timeout.Milliseconds() != 5000 {
		t.Errorf("Timeout = %v, want 5s", got.Timeout)
	}
}

// TestLoadDisabledConfig verifies the disabled flag short-circuits
// resolution even when language defaults would otherwise apply.
func TestLoadDisabledConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, ".project"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "Makefile"), []byte("run:\n\techo go\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".project", "run.json"),
		[]byte(`{"disabled": true}`), 0o644)

	got := Load(dir)
	if !got.Skipped {
		t.Errorf("Skipped = false, want true")
	}
	if got.SkipReason == "" {
		t.Errorf("SkipReason empty; want non-empty")
	}
}

// TestLoadDetectsMakeSmoke verifies a Makefile with a smoke target
// resolves to "make smoke" without an explicit config.
func TestLoadDetectsMakeSmoke(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "Makefile"),
		[]byte(".PHONY: smoke run\n\nsmoke:\n\techo smoking\n\nrun:\n\techo running\n"), 0o644)

	got := Load(dir)
	if got.Skipped {
		t.Errorf("Skipped = true, want false")
	}
	if got.Command != "make smoke" {
		t.Errorf("Command = %q, want %q", got.Command, "make smoke")
	}
	if got.Source != "make-smoke" {
		t.Errorf("Source = %q, want make-smoke", got.Source)
	}
}

// TestLoadDetectsMakeRun verifies fallback to "make run" when no
// smoke target exists but a run target does.
func TestLoadDetectsMakeRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "Makefile"),
		[]byte(".PHONY: run\n\nrun:\n\techo running\n"), 0o644)

	got := Load(dir)
	if got.Source != "make-run" {
		t.Errorf("Source = %q, want make-run", got.Source)
	}
	if got.Command != "make run" {
		t.Errorf("Command = %q, want make run", got.Command)
	}
}

// TestLoadSkipsWhenNothingDetected verifies a project with no Makefile
// and no language markers reports Skipped with a reason.
func TestLoadSkipsWhenNothingDetected(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	got := Load(dir)
	if !got.Skipped {
		t.Errorf("Skipped = false, want true")
	}
	if got.SkipReason == "" {
		t.Errorf("SkipReason empty; want diagnostic message")
	}
}

// TestHasMakefileTargetIgnoresRecipeLines verifies a target name that
// only appears as part of a recipe (tab-prefixed) does not register
// as a target. Without this, "echo run:" inside a recipe would falsely
// match.
func TestHasMakefileTargetIgnoresRecipeLines(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mk := "" +
		"build:\n" +
		"\techo run: not a target\n"
	_ = os.WriteFile(filepath.Join(dir, "Makefile"), []byte(mk), 0o644)

	if hasMakefileTarget(dir, "run") {
		t.Errorf("hasMakefileTarget(run) = true; recipe-line false-positive")
	}
}

// TestLoadInvalidJSONFallsBackToDefaults verifies a syntactically
// broken run.json does not block smoke resolution — the developer
// gets the language default rather than a hard failure.
func TestLoadInvalidJSONFallsBackToDefaults(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, ".project"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, ".project", "run.json"), []byte("{not json"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "Makefile"),
		[]byte("run:\n\techo run\n"), 0o644)

	got := Load(dir)
	if got.Skipped {
		t.Errorf("Skipped = true on broken config; want fallback")
	}
	if got.Source != "make-run" {
		t.Errorf("Source = %q, want make-run", got.Source)
	}
}
