package lint

import (
	"context"
	"errors"
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
//   - File and directory paths are passed to the shell as discrete positional
//     parameters ($1, $2), never interpolated into the command string. A path
//     containing spaces or shell metacharacters is therefore safe and is no
//     longer silently skipped (the prior safeForShell denylist is gone).
type RawLinter struct {
	// Command is the shell command template, run via `sh -c`. The {file} and
	// {dir} placeholders are rewritten to the positional parameters "$1" and
	// "$2"; the real paths are supplied to sh as discrete arguments, so they
	// are never re-parsed for metacharacters.
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
// Injection safety: the {file}/{dir} placeholders are rewritten to the
// positional shell parameters "$1"/"$2" (see substituteArgs), and the real
// paths are handed to `sh -c` as discrete trailing arguments. Because the
// paths never appear in the command string the shell cannot re-parse them for
// metacharacters, so a hostile or space-containing file name is safe — there
// is no longer any path to skip or reject.
func (r *RawLinter) Run(ctx context.Context, projectRoot, dir string, files []string) Result {
	if r.Command == "" {
		return Result{Error: errors.New("raw linter: empty command")}
	}

	timeout := r.Timeout
	if timeout == 0 {
		timeout = defaultLintTimeout
	}

	cmdStr := substituteArgs(r.Command)
	hasFilePlaceholder := strings.Contains(r.Command, "{file}")

	// Per-file dispatch: one invocation per edited file. Findings aggregate.
	// Error reporting is first-wins for Result.Error — the agent-facing
	// banner renders the error inline, so a joined multi-line error would
	// produce garbled UI. Subsequent errors are logged via slog so they are
	// not silently swallowed; operators can correlate via timestamps.
	if hasFilePlaceholder {
		var agg Result
		for _, f := range files {
			res := runRawCommand(ctx, projectRoot, cmdStr, timeout, f, dir, f)
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

	res := runRawCommand(ctx, projectRoot, cmdStr, timeout, "", dir, "")
	stampLinter(res.Findings, r.Name())
	return res
}

// substituteArgs rewrites the {file} and {dir} placeholders to the positional
// shell parameters "$1" and "$2". The actual paths are NOT inserted here — they
// are passed to sh as discrete positional arguments by runRawCommand, so a path
// with spaces or shell metacharacters stays a single, un-re-parsed word. The
// double quotes keep a spaced path from word-splitting when referenced.
func substituteArgs(command string) string {
	command = strings.ReplaceAll(command, "{file}", `"$1"`)
	command = strings.ReplaceAll(command, "{dir}", `"$2"`)
	return command
}

// runRawCommand executes a single shell command and classifies its output.
// file and dir are passed to sh as the positional parameters $1 and $2 (which
// the command string references via the rewritten "$1"/"$2" placeholders); they
// are discrete arguments, so the shell never re-parses them for metacharacters.
// fallbackPath is used as the Path of an unstructured Finding when the
// output has no parseable diagnostic lines but exit was non-zero; empty
// string leaves the Path unset.
func runRawCommand(parent context.Context, projectRoot, cmdStr string, timeout time.Duration, file, dir, fallbackPath string) Result {
	runCtx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	// `sh -c <script> sh <file> <dir>`: the argument after the script becomes
	// $0 ("sh"), then file is $1 and dir is $2. Passing the paths positionally
	// keeps them out of the interpolated command string — injection-safe.
	cmd := exec.CommandContext(runCtx, "sh", "-c", cmdStr, "sh", file, dir)
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

// stampLinter sets Linter on every finding. Used by adapters so downstream
// code can attribute diagnostics without per-adapter branching.
func stampLinter(findings []Finding, name string) {
	for i := range findings {
		if findings[i].Linter == "" {
			findings[i].Linter = name
		}
	}
}
