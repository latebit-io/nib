package lint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// defaultLintTimeout is the per-command timeout for linter adapters that
// do not override it. Linters typically finish in under 5s on a single
// package; 30s absorbs cold caches and I/O stalls without masking hangs.
const defaultLintTimeout = 30 * time.Second

// maxLintOutputBytes caps stdout+stderr capture per linter invocation so
// a runaway linter cannot balloon memory. 8 MiB fits even very noisy
// runs across a whole monorepo.
const maxLintOutputBytes = 8 << 20

// GolangciLinter runs golangci-lint with JSON output and parses the
// structured Issues array. Exit code is decoupled from findings via
// --issues-exit-code=0 so crashes (non-zero) are distinguishable from
// "ran successfully with findings" (zero).
type GolangciLinter struct {
	// Binary is the path to the golangci-lint executable. Empty string
	// defaults to "golangci-lint" (found via PATH).
	Binary string
	// Timeout overrides defaultLintTimeout when non-zero.
	Timeout time.Duration
}

// Name implements Linter.
func (g *GolangciLinter) Name() string { return "golangci-lint" }

// Run implements Linter. It invokes golangci-lint with JSON output under
// `--issues-exit-code=0` and parses the structured result. The adapter
// honors Go module boundaries: it runs from the module directory containing
// `dir`, not from projectRoot, so multi-module repos lint correctly.
// files is ignored — golangci-lint operates at package granularity.
func (g *GolangciLinter) Run(ctx context.Context, projectRoot, dir string, _ []string) Result {
	timeout := g.Timeout
	if timeout == 0 {
		timeout = defaultLintTimeout
	}

	binary := g.Binary
	if binary == "" {
		binary = "golangci-lint"
	}

	workDir, relDir, err := resolveModuleDir(projectRoot, dir)
	if err != nil {
		return Result{Error: fmt.Errorf("golangci-lint: %w", err)}
	}
	target := "./..."
	if relDir != "" && relDir != "." {
		target = "./" + filepath.ToSlash(relDir) + "/..."
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, binary, "run",
		"--output.json.path=stdout",
		"--issues-exit-code=0",
		target,
	)
	cmd.Dir = workDir
	// Own process group so we can kill all children on timeout.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second

	var stdout, stderr cappedBuffer
	stdout.cap = maxLintOutputBytes
	stderr.cap = maxLintOutputBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	if runCtx.Err() == context.DeadlineExceeded {
		return Result{Error: fmt.Errorf("golangci-lint: timed out after %s", timeout)}
	}

	// With --issues-exit-code=0, any non-zero exit signals an infrastructure
	// failure (bad config, parse error, etc.) — never "found violations."
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = strings.TrimSpace(stdout.String())
			}
			if msg == "" {
				msg = runErr.Error()
			}
			return Result{Error: fmt.Errorf("golangci-lint: exit %d: %s", exitErr.ExitCode(), firstLine(msg))}
		}
		return Result{Error: fmt.Errorf("golangci-lint: %w", runErr)}
	}

	findings, err := parseGolangciJSON(stdout.Bytes())
	if err != nil {
		preview := firstLine(strings.TrimSpace(stdout.String()))
		return Result{Error: fmt.Errorf("golangci-lint: parse: %v (output preview: %s)", err, preview)}
	}

	findings = reparentFindings(projectRoot, workDir, findings)
	for i := range findings {
		findings[i].Linter = g.Name()
	}
	return Result{Findings: findings}
}

// golangciReport is the subset of the golangci-lint JSON schema we consume.
// Unknown fields are ignored by encoding/json; we depend only on Issues.
type golangciReport struct {
	Issues []golangciIssue `json:"Issues"`
}

type golangciIssue struct {
	FromLinter string      `json:"FromLinter"`
	Text       string      `json:"Text"`
	Pos        golangciPos `json:"Pos"`
}

type golangciPos struct {
	Filename string `json:"Filename"`
	Line     int    `json:"Line"`
	Column   int    `json:"Column"`
}

// parseGolangciJSON decodes the stdout from `golangci-lint run
// --output.json.path=stdout` into a flat []Finding. Uses a streaming decoder
// so trailing non-JSON summary text (golangci-lint v2 appends lines like
// "0 issues." after the JSON object) does not trip the parse.
//
// Returns an error only when the bytes are not valid JSON with an "Issues"
// field — an empty/absent Issues array is a clean run.
func parseGolangciJSON(data []byte) ([]Finding, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		// golangci-lint sometimes exits with zero output on clean runs when
		// no linters are enabled. Treat as clean rather than an error.
		return nil, nil
	}
	var report golangciReport
	if err := json.NewDecoder(bytes.NewReader(trimmed)).Decode(&report); err != nil {
		return nil, err
	}
	findings := make([]Finding, 0, len(report.Issues))
	for _, iss := range report.Issues {
		msg := iss.Text
		if iss.FromLinter != "" {
			msg = fmt.Sprintf("%s (%s)", iss.Text, iss.FromLinter)
		}
		findings = append(findings, Finding{
			Path:    iss.Pos.Filename,
			Line:    iss.Pos.Line,
			Col:     iss.Pos.Column,
			Message: msg,
		})
	}
	return findings, nil
}

// firstLine returns the first non-empty line of s, trimmed. Used to keep
// error messages short when the underlying tool emits multi-line diagnostics.
func firstLine(s string) string {
	for ln := range strings.SplitSeq(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			return ln
		}
	}
	return ""
}

// reparentFindings re-roots each Finding.Path so it is relative to
// projectRoot instead of workDir. Needed when the linter runs from a
// nested module directory in a multi-module repository — its output paths
// are relative to that module, but taskEdits use projectRoot-relative
// paths, so without reparenting the edited-vs-sibling filter would miss.
func reparentFindings(projectRoot, workDir string, findings []Finding) []Finding {
	if projectRoot == "" || workDir == "" || workDir == projectRoot {
		return findings
	}
	offset, err := filepath.Rel(projectRoot, workDir)
	if err != nil || offset == "." {
		return findings
	}
	for i := range findings {
		p := findings[i].Path
		if p == "" || filepath.IsAbs(p) {
			if filepath.IsAbs(p) {
				if rel, relErr := filepath.Rel(projectRoot, p); relErr == nil {
					findings[i].Path = rel
				}
			}
			continue
		}
		findings[i].Path = filepath.ToSlash(filepath.Join(offset, p))
	}
	return findings
}

// resolveModuleDir walks up from projectRoot/dir looking for a go.mod file.
// Returns the module directory (working directory for the linter) and the
// path of dir relative to that module. This keeps multi-module repos working
// even though callers only know the project-relative package path.
func resolveModuleDir(projectRoot, dir string) (workDir, relDir string, err error) {
	if projectRoot == "" {
		return "", dir, nil
	}
	abs := filepath.Join(projectRoot, dir)
	cur := abs
	for {
		if _, e := os.Stat(filepath.Join(cur, "go.mod")); e == nil {
			rel, relErr := filepath.Rel(cur, abs)
			if relErr != nil {
				return "", "", relErr
			}
			return cur, rel, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur || len(parent) < len(projectRoot) {
			// Walked past the project boundary without finding go.mod. Fall
			// back to running from projectRoot — mirrors legacy behavior.
			rel := dir
			return projectRoot, rel, nil
		}
		cur = parent
	}
}

// cappedBuffer is a bytes.Buffer with a soft byte cap. Writes past the cap
// are silently dropped so a runaway linter cannot exhaust memory. The cap
// is advisory — the buffer will exceed it by up to one Write.
type cappedBuffer struct {
	buf bytes.Buffer
	cap int
}

// Write appends p up to the remaining cap. Always reports len(p) written to
// avoid tripping exec.Cmd's short-write detection.
func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.cap > 0 && c.buf.Len() >= c.cap {
		return len(p), nil
	}
	if c.cap > 0 && c.buf.Len()+len(p) > c.cap {
		take := c.cap - c.buf.Len()
		c.buf.Write(p[:take])
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

// Bytes returns the accumulated bytes without copying.
func (c *cappedBuffer) Bytes() []byte { return c.buf.Bytes() }

// String returns the accumulated bytes as a string.
func (c *cappedBuffer) String() string { return c.buf.String() }
