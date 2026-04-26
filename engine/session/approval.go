package session

import (
	"errors"
	"strings"

	"github.com/latebit-io/junto/engine/buffer"
	"github.com/latebit-io/junto/engine/editor"
)

// Edit-approval flow for Session.
//
// The engine enforces a two-step review contract:
//
//  1. ReviewEdit  — frontend computes and presents the diff to the developer.
//  2. ApproveEdit — frontend passes the (possibly modified) replacement text.
//
// ApproveEdit fails if ReviewEdit was not called first. This guarantees
// that every frontend — TUI, GUI, web — shows the developer what the
// agent proposes before anything is applied. No blind approvals.
//
// The "two-step approval" variants (PrepareApproval / CompleteApproval /
// AbortApproval / ApproveAndContinue) decouple "produce the plan" from
// "signal the agent" so the frontend can apply the edit and signal the
// agent as separate, atomic steps. This file covers both variants — the
// state they touch (pendingEdit, stagedEditFile, editReviewed,
// pendingContinue*) is shared.
//
// State remains on Session (the fields straddle approval, file I/O, and
// agent-event handling). The methods are grouped here so the SRP
// boundary is visible at the file level.

// ReviewEdit computes the diff for the pending edit and marks it as reviewed.
// Frontends MUST call this and present the result before calling ApproveEdit.
// If the pending edit targets a non-active file, the session auto-switches
// to that file so the frontend renders the correct buffer.
// Returns nil if there is no pending edit or the search text has no unique match.
// Returns true for switched if the active editor changed.
func (s *Session) ReviewEdit() (diff *editor.DiffResult, switched bool) {
	if s.pendingEdit == nil {
		return nil, false
	}
	// editorForEdit may auto-open a file that isn't in the map yet.
	e := s.editorForEdit()
	if e == nil {
		return nil, false
	}
	// Auto-switch to the target file so the frontend shows the right buffer.
	// Done after editorForEdit so auto-opened files are also switched to.
	if s.pendingEdit.Path != "" {
		canon := s.CanonPath(s.pendingEdit.Path)
		s.mu.Lock()
		if canon != s.activeFile {
			s.activeEditor = e
			s.activeFile = canon
			switched = true
		}
		s.mu.Unlock()
	}
	diff = e.ComputeDiff(s.pendingEdit.Search, s.pendingEdit.Replace)
	if diff != nil {
		s.editReviewed = true
	}
	return diff, switched
}

// ApproveEdit applies the reviewed edit to the editor buffer.
// The frontend must provide the final search and replace text — typically
// the full affected lines from the diff, with the replacement possibly
// modified by the developer.
//
// Returns (true, "") on success, or (false, reason) on failure.
// Fails if ReviewEdit was not called first.
func (s *Session) ApproveEdit(search, replace string) (bool, string) {
	if s.pendingEdit == nil || !s.HasAgent() {
		return false, "no pending edit"
	}
	if !s.editReviewed {
		return false, "edit not reviewed — call ReviewEdit first"
	}
	e := s.editorForEdit()
	if e == nil {
		s.agent.Reject()
		s.pendingEdit = nil
		s.editReviewed = false
		return false, "file not open"
	}
	editPath := s.activeFile
	if s.pendingEdit.Path != "" {
		editPath = s.CanonPath(s.pendingEdit.Path)
	}
	lineOrigins := computeLineOrigins(search, s.pendingEdit.Replace, replace)
	ok, reason := e.ApplyEdit(search, replace, lineOrigins)
	proposedReplace := s.pendingProposedReplace
	editID := s.pendingEdit.ID
	if ok {
		s.lastEditedFile = editPath
		s.mu.Lock()
		s.modifiedFiles[editPath] = true
		s.mu.Unlock()
		// Stash the post-apply buffer content so the subsequent
		// Continue() call can detect developer edits made to the
		// buffer between approval and continue (continue-diff capture).
		s.pendingContinuePath = editPath
		s.pendingContinueExpected = e.Buf.Content()
		s.pendingContinueSet = true
		s.agent.Approve()
		modified := replace != proposedReplace
		accepted := map[string]any{
			"id":               editID,
			"path":             editPath,
			"search":           search,
			"replace":          replace,
			"modified_by_user": modified,
		}
		if modified {
			accepted["proposed_replace"] = proposedReplace
		}
		s.emitCapture("accepted", accepted)
	} else {
		s.agent.Reject()
		s.emitCapture("rejected", map[string]any{
			"id":     editID,
			"path":   editPath,
			"reason": reason,
			"source": "apply_failed",
		})
	}
	s.pendingEdit = nil
	s.pendingProposedReplace = ""
	s.editReviewed = false
	return ok, reason
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
	LineOrigins []*editor.LineOrigin
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
	e := s.editorForEdit()
	if e == nil {
		s.agent.Reject()
		s.pendingEdit = nil
		s.editReviewed = false
		return nil, errors.New("file not open")
	}
	loc, reason := e.LocateEdit(search)
	if loc == nil {
		s.agent.Reject()
		s.pendingEdit = nil
		s.editReviewed = false
		return nil, errors.New(reason)
	}
	if s.pendingEdit.Path != "" {
		s.stagedEditFile = s.CanonPath(s.pendingEdit.Path)
	} else {
		s.stagedEditFile = s.activeFile
	}
	lineOrigins := computeLineOrigins(search, s.pendingEdit.Replace, replace)

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
// Call this after the frontend has applied the edit. Promotes the staged
// edit path to lastEditedFile so Continue sends the right content.
//
// Sets awaitingContinue eagerly — the agent will shortly emit
// StatusEditing on the best-effort event path, but that event can be dropped
// under channel pressure. Gating on our own action keeps CanContinue honest
// even when the event is lost.
func (s *Session) CompleteApproval() {
	if s.HasAgent() {
		s.lastEditedFile = s.stagedEditFile
		if s.stagedEditFile != "" {
			s.mu.Lock()
			s.modifiedFiles[s.stagedEditFile] = true
			s.mu.Unlock()
		}
		s.stagedEditFile = ""
		s.mu.Lock()
		s.awaitingContinue = true
		s.mu.Unlock()
		s.agent.Approve()
	}
}

// AbortApproval rejects a prepared approval that was never completed.
// Use this when the apply step fails after PrepareApproval. The agent
// receives a rejection and can try a different approach — unlike
// CancelAgent which kills the entire run.
func (s *Session) AbortApproval() {
	if s.HasAgent() {
		s.stagedEditFile = ""
		s.agent.Reject()
	}
}

// RejectEdit rejects the pending edit and signals the agent. The
// source string is captured into the session journal so post-mortem
// analysis can distinguish a developer-driven reject (Esc keypress)
// from a system-driven auto-reject (search-text mismatch, missing
// editor, etc.). Pre-fix this always recorded "user" regardless of
// caller, which made auto-reject silent failures appear as if the
// developer had intervened — a meaningful UX-debugging hazard.
//
// Callers in the TUI:
//   - ActionAgentReject (Esc keypress)   → source "user"
//   - EditProposed search-mismatch path  → source "search-mismatch"
//   - ApproveEdit "file not open" path   → source "file-not-open"
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
	s.pendingContinuePath = ""
	s.pendingContinueExpected = ""
	s.pendingContinueSet = false
	s.editReviewed = false
	s.agent.Reject()
}

// ApproveAndContinue atomically approves the edit and continues the agent.
// Use this when the agent should proceed immediately after approval (e.g.
// instant-apply, auto-continue at higher autonomy levels). Serialising both
// steps inside the session avoids the TUI orchestrating multi-step engine
// transitions.
func (s *Session) ApproveAndContinue() {
	s.CompleteApproval()
	s.Continue()
}
