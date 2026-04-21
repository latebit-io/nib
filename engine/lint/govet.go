package lint

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// GoVetLinter is a fallback adapter used when golangci-lint is not
// installed. It shells out to `go vet` and parses the text-line output.
//
// Signal handling: go vet does not have a --issues-exit-code equivalent,
// so exit code alone cannot distinguish "found issues" from "failed to
// load package." The adapter instead parses lines matching
// `<path>:<line>:<col>: <message>` as findings; a non-zero exit with
// zero parseable findings is treated as an infrastructure error.
type GoVetLinter struct {
	// Binary is the path to the go executable. Empty string defaults to "go".
	Binary string
	// Timeout overrides defaultLintTimeout when non-zero.
	Timeout time.Duration
}

// Name implements Linter.
func (*GoVetLinter) Name() string { return "go vet" }

// vetLineRE matches `<path>:<line>:<col>: <message>`. Path is any non-colon
// sequence — go vet never emits Windows drive letters in the paths we lint
// (all targets are module-relative), so a greedy no-colon path is safe.
var vetLineRE = regexp.MustCompile(`^([^:]+):(\d+):(\d+):\s*(.+)$`)

// Run implements Linter. It invokes `go vet` from the module directory
// containing dir and parses the text output for findings. files is ignored
// — go vet operates at package granularity.
func (g *GoVetLinter) Run(ctx context.Context, projectRoot, dir string, _ []string) Result {
	timeout := g.Timeout
	if timeout == 0 {
		timeout = defaultLintTimeout
	}

	binary := g.Binary
	if binary == "" {
		binary = "go"
	}

	workDir, relDir, err := resolveModuleDir(projectRoot, dir)
	if err != nil {
		return Result{Error: fmt.Errorf("go vet: %w", err)}
	}
	target := "./..."
	if relDir != "" && relDir != "." {
		target = "./" + filepath.ToSlash(relDir) + "/..."
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, binary, "vet", target)
	cmd.Dir = workDir
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
		return Result{Error: fmt.Errorf("go vet: timed out after %s", timeout)}
	}

	// go vet writes findings to stderr; stdout is normally empty. Parse both
	// so an oddly-configured environment or a build-tag banner on stdout does
	// not silently swallow diagnostics. Pass workDir as the path base since
	// go vet emits paths relative to its cwd.
	findings := parseGoVetOutput(workDir, stderr.String())
	findings = append(findings, parseGoVetOutput(workDir, stdout.String())...)
	findings = reparentFindings(projectRoot, workDir, findings)

	// Non-zero exit with no parseable findings means the linter itself failed
	// (missing module, malformed package, `go` not on PATH). Surface as
	// infrastructure error rather than fabricating violations.
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) && len(findings) > 0 {
			// Normal case: vet found issues and exited non-zero.
			return resultWithLinter(findings, "go vet")
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = runErr.Error()
		}
		return Result{Error: fmt.Errorf("go vet: %s", firstLine(msg))}
	}

	return resultWithLinter(findings, "go vet")
}

// parseGoVetOutput scans text output for lines matching `<path>:<line>:<col>:
// <message>` and returns them as Findings. Lines that don't match the
// pattern (progress banners like `# pkg`, blank lines) are ignored.
//
// baseDir is the working directory the linter ran from — absolute paths
// in the output are normalized to baseDir-relative so downstream reparenting
// to projectRoot works uniformly.
func parseGoVetOutput(baseDir, output string) []Finding {
	if output == "" {
		return nil
	}
	var findings []Finding
	scanner := bufio.NewScanner(strings.NewReader(output))
	// Raise token limit: long vet diagnostics (e.g. generics with many type
	// parameters) can exceed the default 64 KiB.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		m := vetLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		lineNum, _ := strconv.Atoi(m[2])
		colNum, _ := strconv.Atoi(m[3])
		findings = append(findings, Finding{
			Path:    normalizePath(baseDir, m[1]),
			Line:    lineNum,
			Col:     colNum,
			Message: strings.TrimSpace(m[4]),
		})
	}
	return findings
}

// normalizePath returns a baseDir-relative path when possible, or the
// original path otherwise. Preserves the path unchanged if baseDir is empty
// (tests that don't set a root).
func normalizePath(baseDir, path string) string {
	if baseDir == "" {
		return path
	}
	// Handle common "./file.go" form emitted by go vet.
	path = strings.TrimPrefix(path, "./")
	if filepath.IsAbs(path) {
		if rel, err := filepath.Rel(baseDir, path); err == nil {
			return rel
		}
	}
	return path
}

// resultWithLinter stamps Linter on every finding so downstream formatters
// need no per-adapter branching.
func resultWithLinter(findings []Finding, name string) Result {
	for i := range findings {
		findings[i].Linter = name
	}
	return Result{Findings: findings}
}
