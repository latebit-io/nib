package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// defaultLintTimeout is the maximum duration a single lint command can run.
const defaultLintTimeout = 30 * time.Second

// safeForShell reports whether s is safe to interpolate into a shell command.
// Rejects paths containing shell metacharacters that could enable injection.
func safeForShell(s string) bool {
	for _, c := range s {
		switch c {
		case '\'', '"', '`', '$', '\\', ';', '&', '|', '(', ')', '<', '>', '\n', '\r', '\t', ' ', '*', '?', '[', ']', '{', '}', '~', '!', '#':
			return false
		}
	}
	return s != ""
}

// runStyleLint executes configured lint commands against the edited file
// and returns formatted output for injection into the agent's context.
// Returns empty string when there are no violations or no commands configured.
// Skips execution if the file path contains shell metacharacters.
func (a *Agent) runStyleLint(ctx context.Context, relPath string) string {
	a.mu.Lock()
	cmds := a.styleLintCmd
	a.mu.Unlock()

	if len(cmds) == 0 {
		return ""
	}

	dir := filepath.Dir(relPath)
	if !safeForShell(relPath) || !safeForShell(dir) {
		slog.Warn("style lint: skipping — file path contains shell metacharacters", "path", relPath)
		return "[style lint skipped: file path contains shell metacharacters]"
	}

	timeout := a.lintTimeout
	if timeout == 0 {
		timeout = defaultLintTimeout
	}

	projectRoot := a.workspace.ProjectRoot()
	var parts []string

	// Base filename for filtering — lint output typically prefixes lines with
	// the file path. We filter to only show violations from the edited file,
	// not from other files in the same package directory.
	baseName := filepath.Base(relPath)

	for _, cmdTemplate := range cmds {
		cmdStr := strings.ReplaceAll(cmdTemplate, "{file}", relPath)
		cmdStr = strings.ReplaceAll(cmdStr, "{dir}", dir)
		output := runLintCommand(ctx, projectRoot, cmdStr, timeout)
		// When linting a directory ({dir}), filter to only show violations
		// from the edited file — other files in the package are noise.
		if output != "" && strings.Contains(cmdTemplate, "{dir}") {
			output = filterLintOutput(output, relPath, baseName)
		}
		if output != "" {
			// Show the template with {file} placeholder, not the expanded command,
			// to avoid leaking private paths or inline credentials from user config.
			parts = append(parts, fmt.Sprintf("$ %s\n%s", cmdTemplate, output))
		}
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n")
}

// filterLintOutput keeps only lines that reference the edited file.
// Linters typically prefix each violation with "path/to/file.go:line:col:".
// Lines that don't match any known prefix (summary lines, blank lines) are
// kept if at least one file-specific line was found.
func filterLintOutput(output, relPath, baseName string) string {
	lines := strings.Split(output, "\n")
	var filtered []string
	for _, line := range lines {
		// Match full relative path or just the base filename.
		if strings.Contains(line, relPath) || strings.HasPrefix(line, baseName+":") {
			filtered = append(filtered, line)
		}
	}
	return strings.TrimSpace(strings.Join(filtered, "\n"))
}

// runLintCommand executes a single lint command and returns its output.
// Returns empty string on success (no output) or when the command produces
// no stdout/stderr. Non-zero exit codes are expected from linters that
// find violations — the output is returned regardless of exit code.
func runLintCommand(parent context.Context, projectRoot, cmdStr string, timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.Dir = projectRoot

	// Run in its own process group so we can kill all children on timeout.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second

	htw := newHeadTailWriter(maxBashHead, maxBashTail)
	cmd.Stdout = htw
	cmd.Stderr = htw

	err := cmd.Run()
	output := strings.TrimSpace(htw.String())

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			slog.Warn("style lint: command timed out", "command", cmdStr, "timeout", timeout)
			if output == "" {
				return fmt.Sprintf("[timed out after %s]", timeout)
			}
			return output + fmt.Sprintf("\n[timed out after %s]", timeout)
		}
		// Non-zero exit is normal for linters that find violations.
		slog.Debug("style lint: command exited with error", "command", cmdStr, "err", err)
	}

	return output
}
