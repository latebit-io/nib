// Package command holds the reference TUI's slash-command
// implementations and its [kit/command.Session] adapter. The
// framework — Registry, Definition, parser, /help — lives in
// kit/command; this package contributes the frontend-shaped
// commands and the per-dispatch session that bridges them to TUI
// view state.
//
// Layering: tui/command depends only on kit/command. It must not
// import tui/ui — tui/ui depends on tui/command (e.g. agent_pane
// constructs a PaneSession at dispatch time), so any reverse
// dependency would cycle. The Pane interface below is the
// decoupling seam.
package command

import (
	"context"

	kitcmd "github.com/latebit-io/nib/kit/command"
)

// Pane is the minimum slice of the agent pane PaneSession writes
// to. tui/ui's *AgentPaneModel satisfies it via AppendMeta. Kept
// as a one-method interface so tui/command does not import tui/ui.
type Pane interface {
	// AppendMeta renders chrome / status text into the transcript
	// without involving the LLM.
	AppendMeta(text string)
}

// PaneSession is a per-dispatch implementation of
// [kitcmd.Session] for the reference TUI. Display writes
// synchronously to the pane; SubmitPrompt records the prompt for
// post-dispatch delivery so the caller can return the appropriate
// tea.Cmd (typically one that emits the existing goal-submitted
// message). PaneSession is single-shot — construct one per
// Registry.Dispatch call, inspect [PaneSession.PendingPrompt]
// afterwards, then discard. Reusing an instance across dispatches
// would silently merge submissions.
type PaneSession struct {
	pane Pane

	pendingPrompt    string
	pendingPromptSet bool
}

// NewPaneSession returns a PaneSession whose Display routes to
// pane.AppendMeta. pane may be nil — Display becomes a no-op,
// which is useful for tests that only exercise SubmitPrompt.
func NewPaneSession(pane Pane) *PaneSession {
	return &PaneSession{pane: pane}
}

// Display routes to Pane.AppendMeta synchronously. AppendMeta
// owns trailing-newline normalisation; PaneSession does not
// transform the text.
func (s *PaneSession) Display(text string) {
	if s.pane != nil {
		s.pane.AppendMeta(text)
	}
}

// SubmitPrompt records the prompt for post-dispatch delivery and
// always returns nil; actual delivery is the caller's
// responsibility via [PaneSession.PendingPrompt]. A second
// SubmitPrompt call within the same dispatch overwrites the first
// — Phase 1 commands do not multi-submit, so this is reserved as
// a programmer-error signal (callers that care can check
// [PaneSession.PendingPrompt] is unset before invoking Dispatch).
func (s *PaneSession) SubmitPrompt(_ context.Context, text string) error {
	s.pendingPrompt = text
	s.pendingPromptSet = true
	return nil
}

// PendingPrompt returns (text, true) when a command called
// SubmitPrompt during dispatch, else ("", false). The caller is
// responsible for translating this into a frontend message
// (e.g., emitting a GoalSubmittedMsg via tea.Cmd).
func (s *PaneSession) PendingPrompt() (string, bool) {
	return s.pendingPrompt, s.pendingPromptSet
}

// Compile-time guarantee that PaneSession satisfies the kit
// Session contract. Drift (e.g., a method renamed in kit/command)
// fails the build instead of failing dispatch at runtime.
var _ kitcmd.Session = (*PaneSession)(nil)
