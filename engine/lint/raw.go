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
//
// Defense-in-depth: the adapter runs via `sh -c`, so any substituted value
// that reaches the command string is shell-interpreted. Both {file} and
// {dir} are validated with safeForShell before substitution. Upstream edit
// approval SHOULD reject exotic paths, but the adapter enforces the
// invariant locally rather than trusting the caller — a slipped character
// here is a command-injection primitive.
func (r *RawLinter) Run(ctx context.Context, projectRoot, dir string, files []string) Result {
	if r.Command == "" {
		return Result{Error: errors.New("raw linter: empty command")}
	}

	timeout := r.Timeout
	if timeout == 0 {
		timeout = defaultLintTimeout
	}

	// filepath.Dir yields "." for root-level files — accepted. Empty dir is
	// unusual but not exploitable; skip the check so it doesn't mask into a
	// "contains metacharacters" error.
	if dir != "" && !safeForShell(dir) {
		slog.Warn("raw linter: dir contains shell metacharacters", "dir", dir)
		return Result{Error: fmt.Errorf("raw linter: dir contains shell metacharacters: %q", dir)}
	}

	hasFilePlaceholder := strings.Contains(r.Command, "{file}")

	// Per-file dispatch: one invocation per edited file. Findings aggregate.
	// Error reporting is first-wins for Result.Error — the agent-facing
	// banner renders the error inline, so a joined multi-line error would
	// produce garbled UI. Subsequent errors are logged via slog so they are
	// not silently swallowed; operators can correlate via timestamps.
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
			if res.Error != nil {
				if agg.Error == nil {
					agg.Error = res.Error
				} else {
					slog.Warn("raw linter: subsequent file failed (not surfaced)", "path", f, "err", res.Error, "first_err", agg.Error)
				}
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

	// Timeout / parent cancellation win over everything. A killed process may
	// have emitted partial diagnostic lines before SIGKILL; returning those as
	// "findings" would present incomplete data as complete, the exact class
	// of misleading signal this package exists to prevent.
	if runCtx.Err() != nil {
		if err := classifyRunError("style lint", parent, runCtx, runErr, timeout, &stdout, &stderr); err != nil {
			return Result{Error: err}
		}
	}

	// Parse both streams — user tools have unpredictable stream conventions.
	// Parseable findings take priority over exit-code classification so raw
	// linters (which often exit non-zero when findings exist) are not
	// mis-reported as infrastructure failures.
	findings := parseGoVetOutput(projectRoot, stdout.String())
	findings = append(findings, parseGoVetOutput(projectRoot, stderr.String())...)
	if len(findings) > 0 {
		return Result{Findings: findings}
	}

	// No parseable findings and context was not cancelled. Classify any
	// remaining run error (non-zero exit, exec error).
	if err := classifyRunError("style lint", parent, runCtx, runErr, timeout, &stdout, &stderr); err != nil {
		// Unstructured-output fallback: if the exit was non-zero but we DO
		// have raw output, surface it as a finding rather than discarding.
		// Parsing already failed above, so we know the text is non-diagnostic
		// form (a summary, a traceback, etc.).
		combined := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
		if combined != "" {
			return Result{Findings: []Finding{{Path: fallbackPath, Message: firstLine(combined)}}}
		}
		return Result{Error: err}
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
