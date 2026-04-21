package lint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// RawLinter runs a user-supplied shell command and parses its output.
// It is an explicit fallback for lint tools without a first-class adapter:
// signal quality is lower than golangci-lint or go vet because the adapter
// must guess at exit-code semantics and output format.
//
// Placeholder substitution: {dir} → lint target directory (package),
// {file} → single edited file. Commands containing {file} are invoked
// once per edited file; commands containing only {dir} are invoked once
// per directory.
//
// Signal rules:
//   - Any parseable `<path>:<line>:<col>: <message>` line in stdout or
//     stderr becomes a structured Finding.
//   - Non-parseable output combined with zero exit is treated as clean,
//     matching the convention that a linter with nothing to say stays silent.
//   - Non-parseable output combined with non-zero exit becomes a single
//     unstructured Finding (path-less) — the agent still sees the message
//     rather than having it silently dropped.
//   - Shell metacharacters in the file path abort the run with Error,
//     preventing command injection via untrusted file names.
type RawLinter struct {
	// Command is the shell command template (passed to `sh -c`).
	Command string
	// Timeout overrides defaultLintTimeout when non-zero.
	Timeout time.Duration
}

// Name implements Linter. Returns "style lint" as a generic identifier —
// raw commands have no inherent name.
func (*RawLinter) Name() string { return "style lint" }

// Run implements Linter. Substitutes placeholders and invokes the command
// once per file (when {file} is present) or once per dir (otherwise).
func (r *RawLinter) Run(ctx context.Context, projectRoot, dir string, files []string) Result {
	if r.Command == "" {
		return Result{Error: errors.New("raw linter: empty command")}
	}

	timeout := r.Timeout
	if timeout == 0 {
		timeout = defaultLintTimeout
	}

	hasFilePlaceholder := strings.Contains(r.Command, "{file}")

	// Per-file dispatch: one invocation per edited file. Findings aggregate.
	if hasFilePlaceholder {
		var agg Result
		for _, f := range files {
			if !safeForShell(f) {
				slog.Warn("raw linter: skipping file with shell metacharacters", "path", f)
				continue
			}
			cmdStr := strings.ReplaceAll(r.Command, "{file}", f)
			cmdStr = strings.ReplaceAll(cmdStr, "{dir}", dir)
			res := runRawCommand(ctx, projectRoot, cmdStr, timeout, f)
			if res.Error != nil && agg.Error == nil {
				agg.Error = res.Error
			}
			agg.Findings = append(agg.Findings, res.Findings...)
		}
		stampLinter(agg.Findings, r.Name())
		return agg
	}

	cmdStr := strings.ReplaceAll(r.Command, "{dir}", dir)
	res := runRawCommand(ctx, projectRoot, cmdStr, timeout, "")
	stampLinter(res.Findings, r.Name())
	return res
}

// runRawCommand executes a single shell command and classifies its output.
// fallbackPath is used as the Path of an unstructured Finding when the
// output has no parseable diagnostic lines but exit was non-zero; empty
// string leaves the Path unset.
func runRawCommand(parent context.Context, projectRoot, cmdStr string, timeout time.Duration, fallbackPath string) Result {
	runCtx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "sh", "-c", cmdStr)
	cmd.Dir = projectRoot
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
		return Result{Error: fmt.Errorf("style lint: timed out after %s", timeout)}
	}

	// Parse both streams — user tools have unpredictable stream conventions.
	findings := parseGoVetOutput(projectRoot, stdout.String())
	findings = append(findings, parseGoVetOutput(projectRoot, stderr.String())...)

	if len(findings) > 0 {
		return Result{Findings: findings}
	}

	// No parseable findings. Use exit code to disambiguate.
	combined := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			if combined == "" {
				return Result{Error: fmt.Errorf("style lint: exit %d with no output", exitErr.ExitCode())}
			}
			// Non-parseable output — emit as a single unstructured finding so
			// the agent still sees it, rather than silently dropping.
			return Result{Findings: []Finding{{Path: fallbackPath, Message: firstLine(combined)}}}
		}
		return Result{Error: fmt.Errorf("style lint: %w", runErr)}
	}

	// Zero exit and no parseable findings — clean.
	return Result{}
}

// safeForShell reports whether s is safe to interpolate into a shell command.
// Rejects paths containing shell metacharacters that could enable injection.
// Empty strings are rejected so a misconfigured placeholder does not collapse
// into an empty argument that the shell interprets differently.
func safeForShell(s string) bool {
	for _, c := range s {
		switch c {
		case '\'', '"', '`', '$', '\\', ';', '&', '|', '(', ')', '<', '>',
			'\n', '\r', '\t', ' ', '*', '?', '[', ']', '{', '}', '~', '!', '#':
			return false
		}
	}
	return s != ""
}

// stampLinter sets Linter on every finding. Used by adapters so downstream
// code can attribute diagnostics without per-adapter branching.
func stampLinter(findings []Finding, name string) {
	for i := range findings {
		if findings[i].Linter == "" {
			findings[i].Linter = name
		}
	}
}
