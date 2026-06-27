package lint

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRawLinter_clean(t *testing.T) {
	r := &RawLinter{Command: "true"}
	res := r.Run(context.Background(), "", "", nil)
	if res.Error != nil {
		t.Fatalf("unexpected error: %v", res.Error)
	}
	if len(res.Findings) != 0 {
		t.Errorf("expected no findings, got %+v", res.Findings)
	}
}

func TestRawLinter_parsesDiagnostics(t *testing.T) {
	r := &RawLinter{Command: `echo "engine/agent/agent.go:42:5: shadowed var"`}
	res := r.Run(context.Background(), "", "", nil)
	if res.Error != nil {
		t.Fatalf("unexpected error: %v", res.Error)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(res.Findings))
	}
	if res.Findings[0].Line != 42 || res.Findings[0].Col != 5 {
		t.Errorf("position parsed wrong: %+v", res.Findings[0])
	}
	if res.Findings[0].Linter != "style lint" {
		t.Errorf("linter stamp missing: %q", res.Findings[0].Linter)
	}
}

func TestRawLinter_nonzeroExitWithOutput(t *testing.T) {
	// Output doesn't match file:line:col, so it becomes an unstructured
	// finding rather than an error — the agent still sees the message.
	r := &RawLinter{Command: `echo "something went sideways"; exit 1`}
	res := r.Run(context.Background(), "", "", nil)
	if res.Error != nil {
		t.Fatalf("unexpected error: %v", res.Error)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("expected 1 unstructured finding, got %d", len(res.Findings))
	}
	if !strings.Contains(res.Findings[0].Message, "something went sideways") {
		t.Errorf("finding message: %q", res.Findings[0].Message)
	}
}

func TestRawLinter_nonzeroExitNoOutput(t *testing.T) {
	r := &RawLinter{Command: "exit 2"}
	res := r.Run(context.Background(), "", "", nil)
	if res.Error == nil {
		t.Fatalf("expected infrastructure error, got clean result")
	}
	if !strings.Contains(res.Error.Error(), "exit 2") {
		t.Errorf("error should mention exit code: %v", res.Error)
	}
}

func TestRawLinter_cancelOverridesPartialFindings(t *testing.T) {
	// Before the fix: a cancelled-mid-output run that had already written
	// parseable diagnostic lines would return those as "findings," masking
	// the cancellation. The craftsmanship rule is that incomplete data must
	// not be presented as complete — cancellation wins over partial findings.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := &RawLinter{
		// Command emits a parseable finding immediately, then hangs. After
		// the parent cancel + SIGKILL, the partial stdout would contain a
		// valid diagnostic line.
		Command: `echo "a.go:1:1: issue"; sleep 30`,
		Timeout: 30 * time.Second,
	}
	res := r.Run(ctx, "", "", nil)
	if res.Error == nil {
		t.Fatal("cancellation must surface as error, not findings")
	}
	if !strings.Contains(res.Error.Error(), "cancel") {
		t.Errorf("error should mention cancellation, got: %v", res.Error)
	}
	if len(res.Findings) != 0 {
		t.Errorf("cancelled run must not expose partial findings, got: %+v", res.Findings)
	}
}

func TestRawLinter_parentCancelled(t *testing.T) {
	// Pre-cancelled parent: the adapter must return promptly with a clear
	// cancellation error, not the generic "exit -1 with no output" signal-kill
	// message. Also verifies no partial findings leak from a killed process.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := &RawLinter{
		Command: "sleep 30",
		Timeout: 30 * time.Second, // far longer than the test deadline
	}

	start := time.Now()
	res := r.Run(ctx, "", "", nil)
	elapsed := time.Since(start)

	// exec.Cmd's WaitDelay is 1s, so a killed process can take up to ~1s
	// to reap. Allow some slack for CI.
	if elapsed > 3*time.Second {
		t.Errorf("cancellation should return promptly, took %s", elapsed)
	}
	if res.Error == nil {
		t.Fatal("cancellation should produce an error result")
	}
	if !strings.Contains(res.Error.Error(), "cancel") {
		t.Errorf("error should mention cancellation, got: %v", res.Error)
	}
	if len(res.Findings) != 0 {
		t.Errorf("cancellation must not produce findings, got: %+v", res.Findings)
	}
}

func TestRawLinter_timeout(t *testing.T) {
	r := &RawLinter{
		Command: "sleep 5",
		Timeout: 100 * time.Millisecond,
	}
	res := r.Run(context.Background(), "", "", nil)
	if res.Error == nil || !strings.Contains(res.Error.Error(), "timed out") {
		t.Errorf("expected timeout error, got: %v", res.Error)
	}
}

func TestRawLinter_emptyCommand(t *testing.T) {
	r := &RawLinter{}
	res := r.Run(context.Background(), "", "", nil)
	if res.Error == nil {
		t.Error("empty command should return error")
	}
}

func TestRawLinter_perFileDispatch(t *testing.T) {
	// The {file} placeholder triggers per-file invocation. Command echoes a
	// diagnostic line mentioning the filename; we should see one finding per
	// file. The placeholder is written unquoted — substituteArgs wraps it in
	// "$1" so the path is passed positionally.
	r := &RawLinter{Command: `echo {file}:1:1: issue`}
	res := r.Run(context.Background(), "", "",
		[]string{"foo.go", "bar.go"})
	if res.Error != nil {
		t.Fatalf("unexpected error: %v", res.Error)
	}
	if len(res.Findings) != 2 {
		t.Fatalf("expected 2 findings (one per file), got %d: %+v", len(res.Findings), res.Findings)
	}
	paths := []string{res.Findings[0].Path, res.Findings[1].Path}
	if paths[0] != "foo.go" || paths[1] != "bar.go" {
		t.Errorf("per-file dispatch wrong: %+v", paths)
	}
}

func TestRawLinter_unsafeDirSafe(t *testing.T) {
	// dir is passed to sh as the positional parameter $2 (referenced via the
	// rewritten "$2"), never interpolated into the command string. A dir with
	// shell metacharacters is therefore handled safely — no rejection, and the
	// metacharacters are not evaluated (no command substitution, no injection).
	// The echoed line carries dir as the path, so we can assert it survived
	// verbatim.
	r := &RawLinter{Command: `echo {dir}:1:1: msg`}
	cases := []string{
		"foo$(whoami)",
		"foo; rm -rf /",
		"foo`id`",
		"foo|bar",
	}
	for _, bad := range cases {
		t.Run(bad, func(t *testing.T) {
			res := r.Run(context.Background(), "", bad, nil)
			if res.Error != nil {
				t.Errorf("dir=%q should be handled safely, got error: %v", bad, res.Error)
			}
			if len(res.Findings) != 1 {
				t.Fatalf("expected 1 finding for dir=%q, got %d: %+v", bad, len(res.Findings), res.Findings)
			}
			if res.Findings[0].Path != bad {
				t.Errorf("dir not passed literally: got %q want %q (metachars were evaluated?)", res.Findings[0].Path, bad)
			}
		})
	}
}

func TestRawLinter_dirDotAccepted(t *testing.T) {
	// filepath.Dir("root-level.go") returns ".". Must not be rejected.
	r := &RawLinter{Command: "true"}
	res := r.Run(context.Background(), "", ".", nil)
	if res.Error != nil {
		t.Errorf("'.' should be accepted as dir, got error: %v", res.Error)
	}
}

func TestRawLinter_pathWithMetacharsSafe(t *testing.T) {
	// Paths with spaces and shell metacharacters are now passed positionally
	// ($1), so they are processed (not skipped) and cannot inject commands.
	// "bad; rm -rf /.go" is echoed literally — the ';' does not run rm.
	r := &RawLinter{Command: `echo {file}:1:1: issue`}
	res := r.Run(context.Background(), "", "",
		[]string{"safe.go", "bad; rm -rf /.go"})
	if len(res.Findings) != 2 {
		t.Fatalf("expected 2 findings (no path skipped), got %d: %+v", len(res.Findings), res.Findings)
	}
	if res.Findings[0].Path != "safe.go" {
		t.Errorf("first path wrong: %q", res.Findings[0].Path)
	}
	// The hostile path is preserved verbatim — proof the shell did not
	// interpret the ';' (no injection, no truncation).
	if res.Findings[1].Path != "bad; rm -rf /.go" {
		t.Errorf("hostile path not passed literally: %q", res.Findings[1].Path)
	}
}
