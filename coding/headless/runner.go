package headless

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/latebit-io/nib/coding/event"
	kithl "github.com/latebit-io/nib/kit/headless"
)

// agentPort is the narrow interface Runner needs from the coding agent.
// Extends [kithl.Agent] (Reply, Cancel) with the coding-specific
// run-start signature and edit-flow controls (Approve, Reject).
// Matches the methods on *agent.Agent used during a headless run.
type agentPort interface {
	kithl.Agent
	Run(ctx context.Context, fileName, fileContent, goal string)
	Approve(content string)
	Reject()
	IsWaiting() bool
}

// Runner drives the coding agent to completion without a TUI. Wraps
// [kithl.Runner] for the generic event-drain + REPL loop, and handles
// coding-specific events (edit application, file-creation tracking,
// lint status) via an [kithl.EventHandler].
type Runner struct {
	inner     *kithl.Runner
	agent     agentPort
	workspace *DiskWorkspace

	// Mutable tracking state populated by handleEvent during Drive.
	// Single-goroutine ownership (the inner runner's drain loop), so
	// no synchronization is required between the handler and the
	// post-Drive read in [Run].
	filesChanged []string
	filesCreated []string

	stderr io.Writer
	isTTY  bool
}

// NewRunner creates a headless runner. Pass os.Stderr for stderr to
// get streaming status output when running interactively.
func NewRunner(agent agentPort, workspace *DiskWorkspace, events <-chan event.Event, stderr io.Writer, isTTY bool) *Runner {
	r := &Runner{
		agent:     agent,
		workspace: workspace,
		stderr:    stderr,
		isTTY:     isTTY,
	}
	r.inner = kithl.New(agent, events,
		kithl.WithHandler(r.handleEvent),
		kithl.WithStderr(stderr),
		kithl.WithTTY(isTTY),
	)
	return r
}

// Run starts the agent with the given goal and blocks until it
// completes. If files are provided, the first is pre-read and passed
// as the active file; the agent reads any others on demand via tools.
func (r *Runner) Run(ctx context.Context, goal string, files []string) *Result {
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

	r.agent.Run(ctx, fileName, fileContent, goal)
	base := r.inner.Drive(ctx)

	return &Result{
		Success:      base.Success,
		Summary:      base.Summary,
		Errors:       base.Errors,
		FilesChanged: r.filesChanged,
		FilesCreated: r.filesCreated,
	}
}

// handleEvent dispatches coding-specific events. Generic events
// (token, status thinking/planning, error, lifecycle) are owned by
// the inner kit runner; this method only sees the fall-through cases.
// Returning a non-nil error appends the message to the inner result's
// Errors slice; the run is not aborted.
func (r *Runner) handleEvent(ctx context.Context, ev event.Event) error {
	switch e := ev.(type) {
	case event.AgentEditProposed:
		return r.applyEdit(e.Edit)

	case event.AgentFileCreated:
		canon := r.workspace.CanonPath(e.Path)
		r.filesCreated = append(r.filesCreated, canon)
		r.status("[created %s]\n", e.Path)
		return nil

	case event.AgentNavigate:
		slog.Debug("navigate ignored in headless mode", "path", e.Path, "line", e.Line)
		return nil

	case event.DiagnosticsUpdated:
		return nil

	case event.AgentStatus:
		switch e.Status {
		case event.StatusLinting:
			r.status("[running style lint...]\n")
		case event.StatusSmoke:
			r.status("[running smoke...]\n")
		}
		return nil
	}

	slog.Debug("unhandled coding event in headless runner", "type", fmt.Sprintf("%T", ev))
	return nil
}

// applyEdit writes the edit directly to disk and approves it so the
// agent advances. Reads raw bytes to preserve the file's trailing
// newline state, performs the replacement on the normalized content
// (matching the agent's view), and restores the original newline
// suffix before writing back. On any failure the edit is rejected so
// the agent recalibrates rather than proceeding with a false "applied
// successfully" signal. Returns the validation/IO error so the kit
// runner records it on the result.
func (r *Runner) applyEdit(edit event.PendingEdit) error {
	raw, absPath, err := r.workspace.ReadFileRaw(edit.Path)
	if err != nil {
		return r.rejectWith(fmt.Sprintf("cannot read %s for edit: %v", edit.Path, err))
	}

	rawStr := string(raw)
	hadTrailingNL := strings.HasSuffix(rawStr, "\n")
	content := strings.TrimSuffix(rawStr, "\n")

	matches := strings.Count(content, edit.Search)
	if matches == 0 {
		return r.rejectWith(fmt.Sprintf("edit search text not found in %s", edit.Path))
	}
	if matches > 1 {
		return r.rejectWith(fmt.Sprintf("edit search text is ambiguous in %s (%d matches)", edit.Path, matches))
	}

	newContent := strings.Replace(content, edit.Search, edit.Replace, 1)

	writeContent := newContent
	if hadTrailingNL {
		writeContent += "\n"
	}

	if err := r.workspace.OverwriteFile(edit.Path, writeContent); err != nil {
		return r.rejectWith(fmt.Sprintf("cannot write %s: %v", edit.Path, err))
	}

	r.filesChanged = append(r.filesChanged, absPath)
	r.status("[edited %s]\n", edit.Path)

	// Approve carries the post-apply content (matching the agent's
	// view: no trailing \n) so the orchestrator's cache reflects the
	// real file state, not the agent's predicted ExpectedContent.
	r.agent.Approve(newContent)
	return nil
}

// rejectWith logs, signals Reject to the agent, and returns an error
// the kit runner records on the result.
func (r *Runner) rejectWith(msg string) error {
	slog.Error(msg)
	r.agent.Reject()
	return errors.New(msg)
}

// status writes a formatted message to stderr in TTY mode. Stderr
// writes are best-effort — errors are intentionally ignored since
// status output is non-critical.
func (r *Runner) status(format string, args ...any) {
	if !r.isTTY {
		return
	}
	_, _ = fmt.Fprintf(r.stderr, format, args...)
}
