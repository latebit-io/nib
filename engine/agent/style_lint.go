package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// styleLintTimeout is the maximum duration a single lint command can run.
const styleLintTimeout = 30 * time.Second

// runStyleLint executes configured lint commands against the edited file
// and returns formatted output for injection into the agent's context.
// Returns empty string when there are no violations or no commands configured.
func (a *Agent) runStyleLint(relPath string) string {
	a.mu.Lock()
	cmds := a.styleLintCmd
	a.mu.Unlock()

	if len(cmds) == 0 {
		return ""
	}

	projectRoot := a.workspace.ProjectRoot()
	var parts []string

	for _, cmdTemplate := range cmds {
		cmdStr := strings.ReplaceAll(cmdTemplate, "{file}", relPath)
		output := runLintCommand(projectRoot, cmdStr)
		if output != "" {
			parts = append(parts, fmt.Sprintf("$ %s\n%s", cmdStr, output))
		}
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n")
}

// runLintCommand executes a single lint command and returns its output.
// Returns empty string on success (no output) or when the command produces
// no stdout/stderr. Non-zero exit codes are expected from linters that
// find violations — the output is returned regardless of exit code.
func runLintCommand(projectRoot, cmdStr string) string {
	ctx, cancel := context.WithTimeout(context.Background(), styleLintTimeout)
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
			slog.Warn("style lint: command timed out", "command", cmdStr, "timeout", styleLintTimeout)
			if output == "" {
				return fmt.Sprintf("[timed out after %s]", styleLintTimeout)
			}
			return output + fmt.Sprintf("\n[timed out after %s]", styleLintTimeout)
		}
		// Non-zero exit is normal for linters that find violations.
		slog.Debug("style lint: command exited with error", "command", cmdStr, "err", err)
	}

	return output
}
