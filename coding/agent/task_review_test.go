package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/engine/lint"
)

// fakeLinter implements lint.Linter with a scripted result for testing
// runTaskReview's three-state pipeline.
type fakeLinter struct {
	name string
	res  lint.Result
	// recordedDirs captures each dir the linter was called with, so tests
	// can assert dedup (one call per unique directory).
	recordedDirs []string
	// recordedFiles captures files[...] for per-file-dispatch assertions.
	recordedFiles [][]string
}

func (f *fakeLinter) Name() string { return f.name }

func (f *fakeLinter) Run(_ context.Context, _, dir string, files []string) lint.Result {
	f.recordedDirs = append(f.recordedDirs, dir)
	cp := append([]string(nil), files...)
	f.recordedFiles = append(f.recordedFiles, cp)
	return f.res
}

// drainTokens reads every queued AgentToken from the channel and returns
// their concatenated text. Non-token events are ignored.
func drainTokens(ch <-chan event.Event) string {
	var b strings.Builder
	for {
		select {
		case ev := <-ch:
			if tok, ok := ev.(event.AgentToken); ok {
				b.WriteString(tok.Text)
			}
		default:
			return b.String()
		}
	}
}

func TestRunTaskReview_NoConfigurationSurfacesBanner(t *testing.T) {
	a := &Agent{bus: newBus(), workspace: promptTestWorkspace{}}
	events := subscribeForTest(t, a)
	a.taskEdits = []taskEdit{{Path: "main.lua", Search: "a", Replace: "b"}}

	msg := a.runTaskReview(context.Background(), "Task completed: X")
	if !strings.Contains(msg, "Task completed: X") {
		t.Errorf("tool message should pass through, got: %s", msg)
	}

	tokens := drainTokens(events)
	if !strings.Contains(tokens, "no lint") || !strings.Contains(tokens, "configured") {
		t.Errorf("expected disambiguating banner, got: %q", tokens)
	}
}

func TestRunTaskReview_NoEditsNoBanner(t *testing.T) {
	a := &Agent{bus: newBus(), workspace: promptTestWorkspace{}}
	events := subscribeForTest(t, a)
	// taskEdits is nil / empty.

	_ = a.runTaskReview(context.Background(), "Done")
	tokens := drainTokens(events)
	if strings.Contains(tokens, "configured") {
		t.Errorf("should not emit banner without edits, got: %q", tokens)
	}
	if strings.Contains(tokens, "Task complete — running style lint") {
		t.Errorf("should not claim to run lint without edits, got: %q", tokens)
	}
}

func TestRunTaskReview_CleanLint(t *testing.T) {
	linter := &fakeLinter{name: "fake", res: lint.Result{}} // no findings, no error
	a := &Agent{
		bus:       newBus(),
		workspace: promptTestWorkspace{},
		linters:   []lint.Linter{linter},
	}
	events := subscribeForTest(t, a)
	a.taskEdits = []taskEdit{{Path: "pkg/foo.go"}}

	_ = a.runTaskReview(context.Background(), "Done")
	tokens := drainTokens(events)
	if !strings.Contains(tokens, "clean ✓") {
		t.Errorf("expected clean banner, got: %q", tokens)
	}
	if strings.Contains(tokens, "violations found") {
		t.Errorf("clean run should not emit violations banner, got: %q", tokens)
	}
	if a.pendingLint != "" {
		t.Errorf("pendingLint should be empty on clean, got: %q", a.pendingLint)
	}
}

func TestRunTaskReview_FindingsOnEditedFileBlocks(t *testing.T) {
	linter := &fakeLinter{name: "fake", res: lint.Result{
		Findings: []lint.Finding{
			{Path: "pkg/foo.go", Line: 10, Col: 5, Linter: "fake", Message: "bad thing"},
		},
	}}
	a := &Agent{
		bus:       newBus(),
		workspace: promptTestWorkspace{},
		linters:   []lint.Linter{linter},
	}
	events := subscribeForTest(t, a)
	a.taskEdits = []taskEdit{{Path: "pkg/foo.go"}}

	_ = a.runTaskReview(context.Background(), "Done")
	tokens := drainTokens(events)
	if !strings.Contains(tokens, "violations found") {
		t.Errorf("expected violations banner, got: %q", tokens)
	}
	if !strings.Contains(a.pendingLint, "pkg/foo.go:10:5") {
		t.Errorf("pendingLint should contain structured finding, got: %q", a.pendingLint)
	}
	if !strings.Contains(a.pendingLint, "[fake]") {
		t.Errorf("pendingLint should tag finding with linter name, got: %q", a.pendingLint)
	}
}

func TestRunTaskReview_SiblingFindingsAreNonBlocking(t *testing.T) {
	linter := &fakeLinter{name: "fake", res: lint.Result{
		Findings: []lint.Finding{
			// Sibling file in same package — not edited by agent.
			{Path: "pkg/sibling.go", Line: 1, Message: "pre-existing issue"},
		},
	}}
	a := &Agent{
		bus:       newBus(),
		workspace: promptTestWorkspace{},
		linters:   []lint.Linter{linter},
	}
	events := subscribeForTest(t, a)
	a.taskEdits = []taskEdit{{Path: "pkg/foo.go"}}

	_ = a.runTaskReview(context.Background(), "Done")
	tokens := drainTokens(events)
	if !strings.Contains(tokens, "clean ✓") {
		t.Errorf("sibling-only findings should not block, got: %q", tokens)
	}
	if !strings.Contains(tokens, "sibling files") {
		t.Errorf("sibling count should be surfaced, got: %q", tokens)
	}
	if a.pendingLint != "" {
		t.Errorf("sibling findings must not populate pendingLint, got: %q", a.pendingLint)
	}
}

func TestRunTaskReview_SiblingFindingsWrittenToReport(t *testing.T) {
	root := t.TempDir()
	linter := &fakeLinter{name: "fake", res: lint.Result{
		Findings: []lint.Finding{
			{Path: "pkg/sibling.go", Line: 7, Col: 2, Linter: "fake", Message: "pre-existing issue A"},
			{Path: "pkg/other.go", Line: 12, Linter: "fake", Message: "pre-existing issue B"},
		},
	}}
	a := &Agent{
		bus:       newBus(),
		workspace: &testWorkspace{root: root},
		linters:   []lint.Linter{linter},
	}
	events := subscribeForTest(t, a)
	a.taskEdits = []taskEdit{{Path: "pkg/foo.go"}}

	_ = a.runTaskReview(context.Background(), "Done")
	tokens := drainTokens(events)

	// Banner stays clean (sibling-only) but now points at an inspectable
	// report by project-relative path, alongside the count.
	relPath := filepath.Join(".project", siblingLintReportName)
	if !strings.Contains(tokens, "clean ✓") {
		t.Errorf("sibling-only findings should not block, got: %q", tokens)
	}
	if !strings.Contains(tokens, "2 pre-existing in sibling files") {
		t.Errorf("banner should carry the sibling count, got: %q", tokens)
	}
	if !strings.Contains(tokens, relPath) {
		t.Errorf("banner should reference the report path %q, got: %q", relPath, tokens)
	}

	// The report file exists and carries every sibling finding in full —
	// the detail the agent could not previously surface.
	data, err := os.ReadFile(filepath.Join(root, ".project", siblingLintReportName))
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	report := string(data)
	for _, want := range []string{"pkg/sibling.go:7:2", "pre-existing issue A", "pkg/other.go:12", "pre-existing issue B"} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q; got:\n%s", want, report)
		}
	}
	// Non-blocking invariant holds: sibling findings never gate the next task.
	if a.pendingLint != "" {
		t.Errorf("sibling findings must not populate pendingLint, got: %q", a.pendingLint)
	}
}

func TestRunTaskReview_InfraErrorDoesNotFakeFindings(t *testing.T) {
	linter := &fakeLinter{name: "fake", res: lint.Result{
		Error: errString("binary not found"),
	}}
	a := &Agent{
		bus:       newBus(),
		workspace: promptTestWorkspace{},
		linters:   []lint.Linter{linter},
	}
	events := subscribeForTest(t, a)
	a.taskEdits = []taskEdit{{Path: "pkg/foo.go"}}

	_ = a.runTaskReview(context.Background(), "Done")
	tokens := drainTokens(events)
	if !strings.Contains(tokens, "fake failed") {
		t.Errorf("infra error should surface adapter-failure banner, got: %q", tokens)
	}
	if strings.Contains(tokens, "violations found") {
		t.Errorf("infra error must NOT trigger violations banner, got: %q", tokens)
	}
	if a.pendingLint != "" {
		t.Errorf("infra error must not inject fake findings, got: %q", a.pendingLint)
	}
}

func TestRunTaskReview_DirDedup(t *testing.T) {
	linter := &fakeLinter{name: "fake", res: lint.Result{}}
	a := &Agent{
		bus:       newBus(),
		workspace: promptTestWorkspace{},
		linters:   []lint.Linter{linter},
	}
	_ = subscribeForTest(t, a)
	// Three edits in one package directory → linter should be called once.
	a.taskEdits = []taskEdit{
		{Path: "pkg/a.go"},
		{Path: "pkg/b.go"},
		{Path: "pkg/c.go"},
	}

	_ = a.runTaskReview(context.Background(), "Done")
	if len(linter.recordedDirs) != 1 {
		t.Errorf("expected 1 linter invocation (dir dedup), got %d: %v", len(linter.recordedDirs), linter.recordedDirs)
	}
	if linter.recordedDirs[0] != "pkg" {
		t.Errorf("expected dir 'pkg', got %q", linter.recordedDirs[0])
	}
	if len(linter.recordedFiles[0]) != 3 {
		t.Errorf("expected all 3 files passed to linter, got %v", linter.recordedFiles[0])
	}
}

func TestRunTaskReview_MultipleDirs(t *testing.T) {
	linter := &fakeLinter{name: "fake", res: lint.Result{}}
	a := &Agent{
		bus:       newBus(),
		workspace: promptTestWorkspace{},
		linters:   []lint.Linter{linter},
	}
	_ = subscribeForTest(t, a)
	a.taskEdits = []taskEdit{
		{Path: "pkg/a/foo.go"},
		{Path: "pkg/b/bar.go"},
	}

	_ = a.runTaskReview(context.Background(), "Done")
	if len(linter.recordedDirs) != 2 {
		t.Errorf("expected 2 linter invocations, got %d: %v", len(linter.recordedDirs), linter.recordedDirs)
	}
}

// errString is a trivial error-from-string for test fixtures.
type errString string

func (e errString) Error() string { return string(e) }
