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

// stdinLine carries one line read from stdin, or an error/EOF signal.
type stdinLine struct {
	text string
	ok   bool // false on EOF or error
}

// Runner drives an agent to completion without a TUI.
// It drains the event channel, auto-approves edits (writing them to disk),
// and collects results. In TTY mode it reads follow-up input from stdin.
type Runner struct {
	agent     agentPort
	workspace *DiskWorkspace
	events    <-chan event.Event
	stdin     io.Reader // injectable for testing; defaults to os.Stdin
	stderr    io.Writer // streaming status for human consumption
	isTTY     bool      // true when stdin is a terminal (enables REPL)

	// lines is the shared stdin reader channel, started once on first
	// REPL read and kept alive for the runner's lifetime. A single
	// goroutine owns the scanner so cancellations don't leak readers.
	lines chan stdinLine
}

// NewRunner creates a headless runner. Pass os.Stderr for stderr to get
// streaming status output when running interactively.
func NewRunner(agent agentPort, workspace *DiskWorkspace, events <-chan event.Event, stderr io.Writer, isTTY bool) *Runner {
	return &Runner{
		agent:     agent,
		workspace: workspace,
		events:    events,
		stdin:     os.Stdin,
		stderr:    stderr,
		isTTY:     isTTY,
	}
}

// startStdinReader launches the single goroutine that reads lines from
// stdin and sends them on r.lines. Called once on the first REPL prompt.
// The goroutine exits naturally when stdin reaches EOF or errors.
func (r *Runner) startStdinReader() {
	r.lines = make(chan stdinLine, 1)
	go func() {
		scanner := bufio.NewScanner(r.stdin)
		for scanner.Scan() {
			text := strings.TrimSpace(scanner.Text())
			r.lines <- stdinLine{text, text != ""}
		}
		if err := scanner.Err(); err != nil {
			slog.Error("stdin read failed", "err", err)
		}
		r.lines <- stdinLine{"", false} // signal EOF
	}()
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
			if done := r.handleEvent(ctx, ev, result, &summary, &summaryTruncated); done {
				return
			}
		}
	}
}

// handleEvent dispatches a single event. Returns true when the run is complete.
func (r *Runner) handleEvent(ctx context.Context, ev event.Event, result *Result, summary *strings.Builder, truncated *bool) bool {
	switch e := ev.(type) {
	case event.AgentToken:
		appendSummary(summary, e.Text, truncated)
		r.status("%s", e.Text)

	case event.AgentStatus:
		r.handleStatus(e)

	case event.AgentToolCall:
		r.status("[%s]\n", e.Name)
		slog.Debug("tool call", "name", e.Name)

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
		return r.handleWaiting(ctx, result, summary, truncated)

	case event.AgentDone:
		result.Success = e.Success
		result.Summary = summary.String()
		return true

	case event.FlushBuffers:
		r.handleFlush(ctx, e)

	case event.DiagnosticsUpdated:
		// No UI to update.

	default:
		slog.Debug("unhandled event in headless runner", "type", fmt.Sprintf("%T", ev))
	}
	return false
}

// handleFlush responds to a FlushBuffers request. In headless mode there are
// no in-memory buffers, so this responds immediately with an empty result.
// Guarded with ctx to avoid blocking if the requester exits.
func (r *Runner) handleFlush(ctx context.Context, e event.FlushBuffers) {
	select {
	case e.Result <- event.FlushResult{Saved: nil, Err: nil}:
	case <-ctx.Done():
	}
}

// appendSummary adds text to the summary builder, truncating if it exceeds
// the byte limit.
func appendSummary(summary *strings.Builder, text string, truncated *bool) {
	if *truncated {
		return
	}
	if summary.Len()+len(text) > maxSummaryBytes {
		summary.WriteString("\n…[summary truncated]")
		*truncated = true
		return
	}
	summary.WriteString(text)
}

// handleStatus writes human-readable status to stderr in TTY mode.
func (r *Runner) handleStatus(e event.AgentStatus) {
	switch e.Status {
	case event.StatusThinking:
		r.status("[thinking...]\n")
	case event.StatusPlanning:
		r.status("[planning...]\n")
	case event.StatusLinting:
		r.status("[running style lint...]\n")
	}
}

// handleWaiting processes the AgentWaiting event.
// In single-shot mode (no TTY), cancels the agent and returns true (done).
// In REPL mode, reads input from stdin and continues the conversation.
// The read is cancellable via ctx so a cancelled run doesn't hang on stdin.
func (r *Runner) handleWaiting(ctx context.Context, result *Result, summary *strings.Builder, truncated *bool) bool {
	result.Summary = summary.String()
	if !r.isTTY {
		r.agent.Cancel()
		result.Success = true
		return true
	}
	input, ok := r.readInput(ctx)
	if !ok {
		r.agent.Cancel()
		result.Success = true
		return true
	}
	// Reset summary for the new turn. In REPL mode, Result.Summary captures
	// the last turn only — prior turns were already streamed to stderr.
	summary.Reset()
	*truncated = false
	if !r.agent.Reply(input) {
		slog.Warn("agent not accepting input, ending conversation")
		result.Success = true
		return true
	}
	return false
}

// applyEdit writes the edit directly to disk and signals the agent to continue.
// Reads raw bytes to preserve the file's trailing newline state, then
// performs the replacement on the normalized content (matching the agent's
// view) and restores the original newline suffix before writing back.
func (r *Runner) applyEdit(edit event.PendingEdit, result *Result) {
	raw, absPath, err := r.workspace.ReadFileRaw(edit.Path)
	if err != nil {
		msg := fmt.Sprintf("cannot read %s for edit: %v", edit.Path, err)
		slog.Error(msg)
		result.Errors = append(result.Errors, msg)
		r.agent.Approve()
		r.agent.Continue(edit.Path, "")
		return
	}

	// Normalize to match the agent's view (ReadFile trims one trailing \n).
	rawStr := string(raw)
	hadTrailingNL := strings.HasSuffix(rawStr, "\n")
	content := strings.TrimSuffix(rawStr, "\n")

	matches := strings.Count(content, edit.Search)
	if matches == 0 {
		msg := fmt.Sprintf("edit search text not found in %s", edit.Path)
		slog.Error(msg)
		result.Errors = append(result.Errors, msg)
		r.agent.Approve()
		r.agent.Continue(edit.Path, content)
		return
	}
	if matches > 1 {
		msg := fmt.Sprintf("edit search text is ambiguous in %s (%d matches)", edit.Path, matches)
		slog.Error(msg)
		result.Errors = append(result.Errors, msg)
		r.agent.Approve()
		r.agent.Continue(edit.Path, content)
		return
	}

	newContent := strings.Replace(content, edit.Search, edit.Replace, 1)

	// Restore the original trailing newline state.
	writeContent := newContent
	if hadTrailingNL {
		writeContent += "\n"
	}

	if err := r.workspace.OverwriteFile(edit.Path, writeContent); err != nil {
		msg := fmt.Sprintf("cannot write %s: %v", edit.Path, err)
		slog.Error(msg)
		result.Errors = append(result.Errors, msg)
		r.agent.Approve()
		r.agent.Continue(edit.Path, content)
		return
	}

	result.FilesChanged = append(result.FilesChanged, absPath)
	r.status("[edited %s]\n", edit.Path)

	// Continue with the normalized content (agent's view, no trailing \n).
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

// readInput reads a single line from the shared stdin reader, cancellable
// via ctx. Returns the trimmed input and true on success, or ("", false)
// on EOF, error, or context cancellation.
func (r *Runner) readInput(ctx context.Context) (string, bool) {
	if r.lines == nil {
		r.startStdinReader()
	}
	r.status("\n> ")

	select {
	case line := <-r.lines:
		return line.text, line.ok
	case <-ctx.Done():
		return "", false
	}
}
