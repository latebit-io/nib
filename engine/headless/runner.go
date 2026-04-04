package headless

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/latebit-io/junto/engine/event"
)

// agentPort is the narrow interface Runner needs from the agent.
// Matches the methods on *agent.Agent used during a headless run.
type agentPort interface {
	Run(ctx context.Context, fileName, fileContent, goal string, contextFiles []string)
	Reply(input string) bool
	Approve()
	Continue(path, bufferContent string)
	Cancel()
	IsWaiting() bool
}

// Runner drives an agent to completion without a TUI.
// It drains the event channel, auto-approves edits (writing them to disk),
// and collects results. In TTY mode it reads follow-up input from stdin.
type Runner struct {
	agent     agentPort
	workspace *DiskWorkspace
	events    <-chan event.Event
	stderr    io.Writer // streaming status for human consumption
	isTTY     bool      // true when stdin is a terminal (enables REPL)
}

// NewRunner creates a headless runner. Pass os.Stderr for stderr to get
// streaming status output when running interactively.
func NewRunner(agent agentPort, workspace *DiskWorkspace, events <-chan event.Event, stderr io.Writer, isTTY bool) *Runner {
	return &Runner{
		agent:     agent,
		workspace: workspace,
		events:    events,
		stderr:    stderr,
		isTTY:     isTTY,
	}
}

// Run starts the agent with the given goal and blocks until it completes.
// If files are provided, the first is pre-read and passed as context.
// Remaining files are listed as context files the agent may edit.
func (r *Runner) Run(ctx context.Context, goal string, files []string) *Result {
	result := &Result{}

	// Pre-read the first file if provided.
	var fileName, fileContent string
	if len(files) > 0 {
		fileName = files[0]
		content, err := r.workspace.ReadFile(fileName)
		if err != nil {
			slog.Warn("headless: cannot pre-read file, agent will read on demand", "path", fileName, "err", err)
			fileName = ""
		} else {
			fileContent = content
		}
	}

	// Start the agent.
	r.agent.Run(ctx, fileName, fileContent, goal, files)

	// Process events until the agent is done.
	r.processEvents(ctx, result)

	return result
}

// maxSummaryBytes caps the in-memory summary buffer. Tokens beyond this
// limit are still streamed to stderr but not persisted in the Result.
const maxSummaryBytes = 10 * 1024 * 1024 // 10 MB

// processEvents drains the event channel, handling each event type.
// Returns when AgentDone is received or the context is cancelled.
func (r *Runner) processEvents(ctx context.Context, result *Result) {
	var summary strings.Builder
	summaryTruncated := false

	for {
		select {
		case <-ctx.Done():
			r.agent.Cancel()
			result.Success = false
			result.Summary = summary.String()
			result.Errors = append(result.Errors, ctx.Err().Error())
			return

		case ev, ok := <-r.events:
			if !ok {
				return
			}
			if done := r.handleEvent(ev, result, &summary, &summaryTruncated); done {
				return
			}
		}
	}
}

// handleEvent dispatches a single event. Returns true when the run is complete.
func (r *Runner) handleEvent(ev event.Event, result *Result, summary *strings.Builder, truncated *bool) bool {
	switch e := ev.(type) {
	case event.AgentToken:
		if !*truncated {
			if summary.Len()+len(e.Text) > maxSummaryBytes {
				summary.WriteString("\n…[summary truncated]")
				*truncated = true
			} else {
				summary.WriteString(e.Text)
			}
		}
		r.status("%s", e.Text)

	case event.AgentStatus:
		r.handleStatus(e)

	case event.AgentToolCall:
		r.status("[%s]\n", e.Name)
		slog.Debug("tool call", "name", e.Name, "args", e.Args)

	case event.AgentEditProposed:
		r.applyEdit(e.Edit, result)

	case event.AgentFileCreated:
		result.FilesCreated = append(result.FilesCreated, r.workspace.CanonPath(e.Path))
		r.status("[created %s]\n", e.Path)

	case event.AgentNavigate:
		slog.Debug("navigate ignored in headless mode", "path", e.Path, "line", e.Line)

	case event.AgentError:
		result.Errors = append(result.Errors, e.Err)
		r.status("error: %s\n", e.Err)

	case event.AgentWaiting:
		return r.handleWaiting(result, summary)

	case event.AgentDone:
		result.Success = e.Success
		result.Summary = summary.String()
		return true

	case event.FlushBuffers:
		// No in-memory buffers in headless mode — respond immediately.
		e.Result <- event.FlushResult{Saved: nil, Err: nil}

	case event.DiagnosticsUpdated:
		// No UI to update.

	default:
		slog.Debug("unhandled event in headless runner", "type", fmt.Sprintf("%T", ev))
	}
	return false
}

// handleStatus writes human-readable status to stderr in TTY mode.
func (r *Runner) handleStatus(e event.AgentStatus) {
	switch e.Status {
	case event.StatusThinking:
		r.status("[thinking...]\n")
	case event.StatusPlanning:
		r.status("[planning...]\n")
	}
}

// handleWaiting processes the AgentWaiting event.
// In single-shot mode (no TTY), cancels the agent and returns true (done).
// In REPL mode, reads input from stdin and continues the conversation.
func (r *Runner) handleWaiting(result *Result, summary *strings.Builder) bool {
	result.Summary = summary.String()
	if !r.isTTY {
		r.agent.Cancel()
		result.Success = true
		return true
	}
	input := r.readInput()
	if input == "" {
		r.agent.Cancel()
		result.Success = true
		return true
	}
	summary.Reset()
	if !r.agent.Reply(input) {
		slog.Warn("agent not accepting input, ending conversation")
		result.Success = true
		return true
	}
	return false
}

// applyEdit writes the edit directly to disk and signals the agent to continue.
func (r *Runner) applyEdit(edit event.PendingEdit, result *Result) {
	content, err := r.workspace.ReadFile(edit.Path)
	if err != nil {
		msg := fmt.Sprintf("cannot read %s for edit: %v", edit.Path, err)
		slog.Error(msg)
		result.Errors = append(result.Errors, msg)
		r.agent.Approve()
		r.agent.Continue(edit.Path, "")
		return
	}

	if !strings.Contains(content, edit.Search) {
		msg := fmt.Sprintf("edit search text not found in %s", edit.Path)
		slog.Error(msg)
		result.Errors = append(result.Errors, msg)
		r.agent.Approve()
		r.agent.Continue(edit.Path, content)
		return
	}

	newContent := strings.Replace(content, edit.Search, edit.Replace, 1)

	if err := r.workspace.OverwriteFile(edit.Path, newContent); err != nil {
		msg := fmt.Sprintf("cannot write %s: %v", edit.Path, err)
		slog.Error(msg)
		result.Errors = append(result.Errors, msg)
		r.agent.Approve()
		r.agent.Continue(edit.Path, content)
		return
	}

	result.FilesChanged = append(result.FilesChanged, r.workspace.CanonPath(edit.Path))
	r.status("[edited %s]\n", edit.Path)

	r.agent.Approve()
	r.agent.Continue(edit.Path, newContent)
}

// status writes a formatted message to stderr when in TTY mode.
// Stderr writes are best-effort — errors are intentionally ignored
// since status output is non-critical.
func (r *Runner) status(format string, args ...any) {
	if !r.isTTY {
		return
	}
	_, _ = fmt.Fprintf(r.stderr, format, args...)
}

// readInput reads a single line from stdin. Returns empty string on EOF.
// I/O errors are logged — they look like EOF to the caller, which ends
// the conversation gracefully rather than crashing.
func (r *Runner) readInput() string {
	r.status("\n> ")
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			slog.Error("stdin read failed", "err", err)
		}
		return ""
	}
	return strings.TrimSpace(scanner.Text())
}
