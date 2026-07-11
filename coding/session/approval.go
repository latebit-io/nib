package session

import (
	"errors"
	"strings"

	"github.com/latebit-io/nib/engine/buffer"
	"github.com/latebit-io/nib/engine/openfile"
)

// Edit-approval flow for Session.
//
// The engine enforces a three-step review contract:
//
//  1. ReviewEdit       — frontend computes and presents the diff to the developer.
//  2. PrepareApproval  — engine validates the (possibly modified) replacement
//     and returns an ApprovalPlan the frontend applies to its buffer.
//  3. CompleteApproval — frontend signals the apply landed; the agent advances.
//     AbortApproval is the counterpart when the apply fails.
//
// PrepareApproval fails if ReviewEdit was not called first. This guarantees
// that every frontend — TUI, GUI, web — shows the developer what the agent
// proposes before anything is applied. No blind approvals.
//
// State remains on Session (the fields straddle approval, file I/O, and
// agent-event handling). The methods are grouped here so the SRP boundary
// is visible at the file level.

// ReviewEdit computes the diff for the pending edit and marks it as reviewed.
// Frontends MUST call this and present the result before calling PrepareApproval.
// If the pending edit targets a non-active file, the session auto-switches
// to that file so the frontend renders the correct buffer.
// Returns nil if there is no pending edit or the search text has no unique match.
// Returns true for switched if the active open file changed.
func (s *Session) ReviewEdit() (diff *openfile.DiffResult, switched bool) {
	if s.pendingEdit == nil {
		return nil, false
	}
	// openFileForEdit may auto-open a file that isn't in the map yet.
	of := s.openFileForEdit()
	if of == nil {
		return nil, false
	}
	// Auto-switch to the target file so the frontend shows the right buffer.
	// Done after openFileForEdit so auto-opened files are also switched to.
	if s.pendingEdit.Path != "" {
		canon := s.CanonPath(s.pendingEdit.Path)
		s.mu.Lock()
		if canon != s.activeFile {
			s.activeOpenFile = of
			s.activeFile = canon
			switched = true
		}
		s.mu.Unlock()
	}
	diff = of.ComputeDiff(s.pendingEdit.Search, s.pendingEdit.Replace)
	if diff != nil {
		s.editReviewed = true
	}
	return diff, switched
}

// stagedApproval is set by PrepareApproval and consumed by CompleteApproval
// to emit the "accepted" capture event after the frontend applies the edit.
type stagedApproval struct {
	editID  string
	search  string
	replace string
}

// ApprovalPlan describes the validated edit the frontend should apply.
type ApprovalPlan struct {
	// Line is the buffer line where the edit starts (0-indexed).
	Line int
	// Col is the buffer column where the edit starts (0-indexed, rune).
	Col int
	// Search is the text to delete from the buffer.
	Search string
	// Replace is the text to insert into the buffer.
	Replace string
	// LineOrigins holds the provenance for each line of the replacement.
	// Index 0 corresponds to the buffer line at Line, index 1 to Line+1, etc.
	// A nil entry means "don't change this line's origin" (the line was
	// unchanged from the search text — the agent just re-included it as context).
	LineOrigins []*openfile.LineOrigin
}

// PrepareApproval validates the reviewed edit and returns an ApprovalPlan.
// The frontend provides the final search/replace (possibly modified in the overlay).
// Clears pending edit state but does NOT mutate the buffer or signal the agent.
//
// Returns an error if there is no pending edit, the edit was not reviewed,
// or the search text cannot be uniquely located in the buffer.
func (s *Session) PrepareApproval(search, replace string) (*ApprovalPlan, error) {
	if s.pendingEdit == nil || !s.HasAgent() {
		return nil, errors.New("no pending edit")
	}
	if !s.editReviewed {
		return nil, errors.New("edit not reviewed — call ReviewEdit first")
	}
	of := s.openFileForEdit()
	if of == nil {
		s.RejectEdit("file-not-open")
		return nil, errors.New("file not open")
	}
	loc, reason := of.LocateEdit(search)
	if reason != "" {
		s.RejectEdit("search-mismatch")
		return nil, errors.New(reason)
	}
	if s.pendingEdit.Path != "" {
		s.stagedEditFile = s.CanonPath(s.pendingEdit.Path)
	} else {
		s.stagedEditFile = s.activeFile
	}
	lineOrigins := computeLineOrigins(search, s.pendingEdit.Replace, replace)

	s.pendingApproval = &stagedApproval{
		editID:  s.pendingEdit.ID,
		search:  search,
		replace: replace,
	}
	s.pendingEdit = nil
	s.editReviewed = false
	return &ApprovalPlan{
		Line:        loc.Line,
		Col:         loc.Col,
		Search:      search,
		Replace:     replace,
		LineOrigins: lineOrigins,
	}, nil
}

// computeLineOrigins determines per-line provenance for a replacement by
// comparing three versions: the original search text, the agent's proposed
// replacement, and the developer's final replacement (possibly modified in
// the overlay). Returns a slice with one entry per line of finalReplace:
//   - nil: line is identical in search and finalReplace — unchanged, keep current origin
//   - OriginAgent: agent changed this line and developer didn't modify it
//   - OriginDeveloper: developer modified this line in the overlay (or added it)
func computeLineOrigins(search, originalReplace, finalReplace string) []*buffer.Origin {
	searchLines := strings.Split(search, "\n")
	origLines := strings.Split(originalReplace, "\n")
	finalLines := strings.Split(finalReplace, "\n")

	origins := make([]*buffer.Origin, len(finalLines))
	for i := range finalLines {
		// Line unchanged from search — agent re-included it as context, skip
		if i < len(searchLines) && searchLines[i] == finalLines[i] {
			continue
		}
		// Line was changed; determine who changed it
		origin := buffer.OriginAgent
		if i >= len(origLines) || origLines[i] != finalLines[i] {
			origin = buffer.OriginDeveloper
		}
		origins[i] = &origin
	}
	return origins
}

// CompleteApproval signals the agent that the prepared edit has been applied.
// Requires PrepareApproval to have run — without staged state we have no
// validated edit to approve, so the call is a no-op (preventing blind
// approvals that would advance the run with no recorded edit).
//
// Promotes the staged edit path to lastEditedFile, marks the file as
// modified, and emits the "accepted" capture event.
func (s *Session) CompleteApproval() {
	if !s.HasAgent() || s.stagedEditFile == "" || s.pendingApproval == nil {
		return
	}
	staged := s.pendingApproval
	editPath := s.stagedEditFile

	s.lastEditedFile = editPath
	s.mu.Lock()
	s.modifiedFiles[editPath] = true
	of := s.openFiles[editPath]
	s.mu.Unlock()

	modified := staged.replace != s.pendingProposedReplace
	accepted := map[string]any{
		"id":               staged.editID,
		"path":             editPath,
		"search":           staged.search,
		"replace":          staged.replace,
		"modified_by_user": modified,
	}
	if modified {
		accepted["proposed_replace"] = s.pendingProposedReplace
	}
	s.emitCapture("accepted", accepted)

	s.stagedEditFile = ""
	s.pendingApproval = nil
	s.pendingProposedReplace = ""
	// Pass the post-apply buffer content so the orchestrator seeds
	// its file cache from the truth. If the file handle is somehow
	// gone (closed/reloaded between PrepareApproval and now) fall
	// back to empty content rather than panicking — Approve still has
	// to fire to unblock the agent.
	var content string
	if of != nil {
		content = of.Content()
	}
	s.agent.Approve(content)
}

// AbortApproval rejects a prepared approval that was never completed.
// Use this when the apply step fails after PrepareApproval. The agent
// receives a rejection and can try a different approach — unlike
// CancelAgent which kills the entire run.
func (s *Session) AbortApproval() {
	if !s.HasAgent() {
		return
	}
	staged := s.pendingApproval
	editPath := s.stagedEditFile
	s.stagedEditFile = ""
	s.pendingApproval = nil
	s.pendingProposedReplace = ""
	if staged != nil {
		s.emitCapture("rejected", map[string]any{
			"id":     staged.editID,
			"path":   editPath,
			"source": "apply_failed",
		})
	}
	s.agent.Reject()
}

// RejectEdit rejects the pending edit and signals the agent. The
// source string is captured into the session journal so post-mortem
// analysis can distinguish a developer-driven reject (Esc keypress)
// from a system-driven auto-reject (search-text mismatch, missing
// editor, etc.). Pre-fix this always recorded "user" regardless of
// caller, which made auto-reject silent failures appear as if the
// developer had intervened — a meaningful UX-debugging hazard.
//
// Callers in the engine and TUI:
//   - ActionAgentReject (Esc keypress)              → source "user"
//   - EditProposed search-mismatch path             → source "search-mismatch"
//   - PrepareApproval "file not open"               → source "file-not-open"
//   - PrepareApproval LocateEdit failure            → source "search-mismatch"
//
// Empty source defaults to "unknown" so the field is always present
// and downstream parsers don't have to special-case missing values.
func (s *Session) RejectEdit(source string) {
	if s.pendingEdit == nil || !s.HasAgent() {
		return
	}
	if source == "" {
		source = "unknown"
	}
	s.emitCapture("rejected", map[string]any{
		"id":     s.pendingEdit.ID,
		"path":   s.pendingEdit.Path,
		"source": source,
	})
	s.pendingEdit = nil
	s.pendingProposedReplace = ""
	s.editReviewed = false
	s.stagedEditFile = ""
	s.pendingApproval = nil
	s.agent.Reject()
}

// ApproveCommand approves the pending bash command and signals the
// agent to run it. No-op when no command is pending — the guard
// prevents a stray approve keystroke from queueing a stale signal on
// the coordinator. Approve carries no content: the post-apply-content
// contract is edit-specific and commands have no buffer to seed.
func (s *Session) ApproveCommand() {
	if s.pendingCommand == nil || !s.HasAgent() {
		return
	}
	s.emitCapture("command_accepted", map[string]any{
		"id":      s.pendingCommand.ID,
		"command": s.pendingCommand.Command,
	})
	s.pendingCommand = nil
	s.agent.Approve("")
}

// AlwaysAllowCommand persists an always-allow rule for the pending bash
// command, then approves it. The rule is exact (see cmdallow.List.Add);
// subsequent identical commands auto-approve at the agent's gate without
// a proposal. When no allowlist is configured or the persist fails, the
// command is still approved once and the error is returned so the
// frontend can surface why the rule did not stick. No-op returning nil
// when nothing is pending.
func (s *Session) AlwaysAllowCommand() error {
	if s.pendingCommand == nil || !s.HasAgent() {
		return nil
	}
	err := s.bashAllow.Add(s.pendingCommand.Command)
	if err == nil {
		s.emitCapture("command_always_allowed", map[string]any{
			"id":      s.pendingCommand.ID,
			"command": s.pendingCommand.Command,
		})
	}
	s.ApproveCommand()
	return err
}

// RejectCommand rejects the pending bash command and signals the agent.
// The source string is captured into the session journal (mirrors
// [Session.RejectEdit]: "user" for an Esc keypress, other values for
// system-driven rejects). Empty source defaults to "unknown".
func (s *Session) RejectCommand(source string) {
	if s.pendingCommand == nil || !s.HasAgent() {
		return
	}
	if source == "" {
		source = "unknown"
	}
	s.emitCapture("command_rejected", map[string]any{
		"id":      s.pendingCommand.ID,
		"command": s.pendingCommand.Command,
		"source":  source,
	})
	s.pendingCommand = nil
	s.agent.Reject()
}
