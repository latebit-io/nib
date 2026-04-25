package runconfig

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// mustWriteFile writes data to path inside the test's temp tree and
// fails the test on any I/O error. Centralised so individual tests
// stay focused on the behavioural assertion rather than fixture
// plumbing — a silent fixture failure would otherwise let a test
// exercise the wrong filesystem state and report a misleading pass.
func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// mustMkdirAll creates dir (and any missing parents) and fails the
// test on error. Same rationale as [mustWriteFile].
func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

// TestLoadExplicitConfig verifies an explicit smoke_command in
// .project/run.json wins over auto-detection.
func TestLoadExplicitConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, ".project"))
	cfg := `{"smoke_command": "echo custom-smoke", "timeout_ms": 5000}`
	mustWriteFile(t, filepath.Join(dir, ".project", "run.json"), []byte(cfg))

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
	mustMkdirAll(t, filepath.Join(dir, ".project"))
	mustWriteFile(t, filepath.Join(dir, "Makefile"), []byte("run:\n\techo go\n"))
	mustWriteFile(t, filepath.Join(dir, ".project", "run.json"),
		[]byte(`{"disabled": true}`))

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
	mustWriteFile(t, filepath.Join(dir, "Makefile"),
		[]byte(".PHONY: smoke run\n\nsmoke:\n\techo smoking\n\nrun:\n\techo running\n"))

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
	mustWriteFile(t, filepath.Join(dir, "Makefile"),
		[]byte(".PHONY: run\n\nrun:\n\techo running\n"))

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
	mustWriteFile(t, filepath.Join(dir, "Makefile"), []byte(mk))

	if hasMakefileTarget(dir, "run") {
		t.Errorf("hasMakefileTarget(run) = true; recipe-line false-positive")
	}
}

// TestLoadGoLibraryModuleSkipped verifies a Go library module
// (go.mod present but no main.go at root) is NOT auto-detected as
// runnable. Without this gate, `go run ./...` would have errored
// with "no Go files" or "main package not found" and the failure
// would have surfaced as a smoke regression instead of a config
// gap. Library modules should configure .project/run.json or a
// Makefile target if smoke applies.
func TestLoadGoLibraryModuleSkipped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.22\n"))
	// Deliberately no main.go — represents a library or a multi-binary
	// repo where the runnable lives under cmd/<name>/main.go.

	got := Load(dir)
	if !got.Skipped {
		t.Errorf("Skipped = false, want true (Go library module without main.go must skip)")
	}
}

// TestLoadGoSinglePackageDetected verifies the single-main-at-root
// pattern resolves to `go run .` (not `go run ./...`). Selecting
// the current package only avoids accumulating sibling packages
// the developer didn't intend to smoke.
func TestLoadGoSinglePackageDetected(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("go"); err != nil {
		// Load's own LookPath gate on `go` returns Skipped when go
		// is missing; this test exercises the positive branch and
		// is only meaningful when go is on PATH.
		t.Skip("go not on PATH; cannot verify the positive go-run branch")
	}

	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.22\n"))
	mustWriteFile(t, filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"))

	got := Load(dir)
	if got.Skipped {
		t.Fatalf("Skipped = true, want false (main.go + go.mod should auto-detect)")
	}
	if got.Command != "go run ." {
		t.Errorf("Command = %q, want %q", got.Command, "go run .")
	}
	if got.Source != "go-run" {
		t.Errorf("Source = %q, want go-run", got.Source)
	}
}

// TestLoadInvalidJSONFallsBackToDefaults verifies a syntactically
// broken run.json does not block smoke resolution — the developer
// gets the language default rather than a hard failure.
func TestLoadInvalidJSONFallsBackToDefaults(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, ".project"))
	mustWriteFile(t, filepath.Join(dir, ".project", "run.json"), []byte("{not json"))
	mustWriteFile(t, filepath.Join(dir, "Makefile"), []byte("run:\n\techo run\n"))

	got := Load(dir)
	if got.Skipped {
		t.Errorf("Skipped = true on broken config; want fallback")
	}
	if got.Source != "make-run" {
		t.Errorf("Source = %q, want make-run", got.Source)
	}
}
