package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/latebit-io/junto/engine/capture"
	"github.com/latebit-io/junto/engine/event"
)

// fakeSink records every Append call so tests can assert on the emitted
// events. Guarded by its own mutex because the session may dispatch from
// multiple goroutines in real use; tests exercise it single-threaded but
// the mutex matches the production contract.
type fakeSink struct {
	mu     sync.Mutex
	events []capture.Event
	err    error
	closed bool
}

func (f *fakeSink) Append(_ context.Context, e capture.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return f.err
}

func (f *fakeSink) Close(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeSink) snapshot() []capture.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capture.Event, len(f.events))
	copy(out, f.events)
	return out
}

func (f *fakeSink) kinds() []string {
	evs := f.snapshot()
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

// TestCaptureDefaultsToNoop verifies that a freshly constructed session has
// a non-nil sink so internal dispatches never panic, and that a session ID
// is generated even when no adapter is wired. This is the null-object
// contract the hot-path call sites depend on.
func TestCaptureDefaultsToNoop(t *testing.T) {
	t.Parallel()

	sess := newTestSession("")
	if sess.sink == nil {
		t.Fatalf("New: sink is nil; NoopSink default not installed")
	}
	if sess.SessionID() == "" {
		t.Errorf("New: SessionID is empty")
	}

	// emitCapture on the default sink must be a no-op that neither
	// panics nor returns an error via the slog path (can only assert
	// "does not panic" here).
	sess.emitCapture("smoke", map[string]any{"k": "v"})
}

// TestSetEventSinkNilFallsBackToNoop verifies that passing nil to the
// setter installs NoopSink rather than storing nil — protecting every
// call site from a mid-lifecycle nil-deref.
func TestSetEventSinkNilFallsBackToNoop(t *testing.T) {
	t.Parallel()

	sess := newTestSession("")
	sess.SetEventSink(nil)
	if sess.sink == nil {
		t.Fatalf("SetEventSink(nil): sink is nil; expected NoopSink fallback")
	}
	if _, ok := sess.sink.(capture.NoopSink); !ok {
		t.Errorf("SetEventSink(nil): sink is %T, want capture.NoopSink", sess.sink)
	}
}

// TestCaptureIntentOnSubmitGoal verifies that SubmitGoal emits a single
// "intent" event carrying the goal text.
func TestCaptureIntentOnSubmitGoal(t *testing.T) {
	t.Parallel()

	sess := newTestSession("")
	sink := &fakeSink{}
	sess.SetEventSink(sink)

	sess.SubmitGoal("fix the bug")

	evs := sink.snapshot()
	if len(evs) != 1 || evs[0].Kind != "intent" {
		t.Fatalf("kinds = %v, want [intent]", sink.kinds())
	}
	got, ok := evs[0].Payload["goal"].(string)
	if !ok || got != "fix the bug" {
		t.Errorf("payload goal = %v, want \"fix the bug\"", evs[0].Payload["goal"])
	}
	if evs[0].SessionID != sess.SessionID() {
		t.Errorf("event SessionID = %q, want %q", evs[0].SessionID, sess.SessionID())
	}
}

// TestCaptureProposalOnEditProposed verifies that HandleEvent emits a
// "proposal" event carrying the edit's identifying fields.
func TestCaptureProposalOnEditProposed(t *testing.T) {
	t.Parallel()

	sess := newTestSession("hello world")
	sink := &fakeSink{}
	sess.SetEventSink(sink)

	sess.HandleEvent(event.AgentEditProposed{Edit: event.PendingEdit{
		ID:      "edit-1",
		Path:    "main.go",
		Search:  "hello",
		Replace: "goodbye",
		Reason:  "reword greeting",
	}})

	evs := sink.snapshot()
	if len(evs) != 1 || evs[0].Kind != "proposal" {
		t.Fatalf("kinds = %v, want [proposal]", sink.kinds())
	}
	if got := evs[0].Payload["id"]; got != "edit-1" {
		t.Errorf("payload id = %v, want edit-1", got)
	}
	if got := evs[0].Payload["path"]; got != "main.go" {
		t.Errorf("payload path = %v, want main.go", got)
	}
}

// TestCaptureRejectedOnRejectEdit verifies that RejectEdit emits a single
// "rejected" event tagged with source=user.
func TestCaptureRejectedOnRejectEdit(t *testing.T) {
	t.Parallel()

	sess := newTestSession("hello world")
	sink := &fakeSink{}
	sess.SetEventSink(sink)

	// Seed a pending edit so RejectEdit's guard passes.
	sess.HandleEvent(event.AgentEditProposed{Edit: event.PendingEdit{
		ID: "edit-2", Path: "main.go", Search: "hello", Replace: "goodbye",
	}})
	sess.RejectEdit("user")

	kinds := sink.kinds()
	if len(kinds) != 2 || kinds[0] != "proposal" || kinds[1] != "rejected" {
		t.Fatalf("kinds = %v, want [proposal rejected]", kinds)
	}
	if got := sink.snapshot()[1].Payload["source"]; got != "user" {
		t.Errorf("rejected source = %v, want user", got)
	}
}

// TestCaptureRejectedSourceDistinguishesAutoFromUser verifies the
// reject capture event preserves the source distinction so post-mortem
// analysis can tell a developer-driven Esc from an auto-reject (search
// mismatch, missing editor, etc.). The Pac-Man rerun surfaced a
// silent auto-reject that was mislabeled as "user" — the developer
// thought they had hit Esc when in fact the search-text-mismatch
// path had fired. This test locks the contract that the source
// string the caller passes is what the capture event records.
func TestCaptureRejectedSourceDistinguishesAutoFromUser(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		source string
		want   string
	}{
		{"user reject", "user", "user"},
		{"search mismatch auto-reject", "search-mismatch", "search-mismatch"},
		{"file not open auto-reject", "file-not-open", "file-not-open"},
		{"empty source defaults to unknown", "", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := newTestSession("hello world")
			sink := &fakeSink{}
			sess.SetEventSink(sink)
			sess.HandleEvent(event.AgentEditProposed{Edit: event.PendingEdit{
				ID: "e", Path: "main.go", Search: "hello", Replace: "x",
			}})
			sess.RejectEdit(tc.source)

			snap := sink.snapshot()
			if len(snap) < 2 {
				t.Fatalf("expected at least 2 events, got %d", len(snap))
			}
			if got := snap[1].Payload["source"]; got != tc.want {
				t.Errorf("rejected source = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCaptureValidatorKindWhenSummariesPresent verifies that proposals
// carrying validator summaries emit a separate "validator" kind event
// after the "proposal" event, so grep-my-history can filter on validator
// activity independently of proposals themselves.
func TestCaptureValidatorKindWhenSummariesPresent(t *testing.T) {
	t.Parallel()

	sess := newTestSession("hello world")
	sink := &fakeSink{}
	sess.SetEventSink(sink)

	sess.HandleEvent(event.AgentEditProposed{
		Edit: event.PendingEdit{ID: "e3", Path: "main.go", Search: "hello", Replace: "hi"},
		ValidatorSummaries: []event.ValidatorSummary{
			{Stage: "go-parse", Verdict: "retry", Feedback: "bad parse"},
			{Stage: "tree-sitter", Verdict: "pass"},
		},
	})

	kinds := sink.kinds()
	if len(kinds) != 2 || kinds[0] != "proposal" || kinds[1] != "validator" {
		t.Fatalf("kinds = %v, want [proposal validator]", kinds)
	}

	payload := sink.snapshot()[1].Payload
	if got := payload["proposal_id"]; got != "e3" {
		t.Errorf("validator event proposal_id = %v, want e3", got)
	}
	stages, ok := payload["stages"].([]map[string]any)
	if !ok || len(stages) != 2 {
		t.Fatalf("stages payload = %+v, want 2 entries", payload["stages"])
	}
	if stages[0]["stage"] != "go-parse" || stages[0]["verdict"] != "retry" {
		t.Errorf("first stage payload = %+v", stages[0])
	}
	if stages[0]["feedback"] != "bad parse" {
		t.Errorf("feedback missing from retry stage: %+v", stages[0])
	}
	if _, has := stages[1]["feedback"]; has {
		t.Errorf("pass stage should not include feedback key: %+v", stages[1])
	}
}

// TestCaptureNoValidatorKindWhenSummariesEmpty verifies that proposals
// without validator summaries emit only the "proposal" event — no empty
// validator record clutters the session log.
func TestCaptureNoValidatorKindWhenSummariesEmpty(t *testing.T) {
	t.Parallel()

	sess := newTestSession("hello world")
	sink := &fakeSink{}
	sess.SetEventSink(sink)

	sess.HandleEvent(event.AgentEditProposed{
		Edit: event.PendingEdit{ID: "e4", Path: "main.go", Search: "x", Replace: "y"},
	})

	kinds := sink.kinds()
	if len(kinds) != 1 || kinds[0] != "proposal" {
		t.Errorf("kinds = %v, want [proposal] only", kinds)
	}
}

// TestCaptureContinueNotModified verifies that a Continue with an
// unchanged buffer still emits a "continue" event, tagged
// was_modified=false and without the full-content fields — so the log
// records the fact without bloating with identical payloads.
func TestCaptureContinueNotModified(t *testing.T) {
	t.Parallel()

	sess := newTestSession("hello world")
	sink := &fakeSink{}
	sess.SetEventSink(sink)

	// Simulate a proposal → review → approve flow.
	sess.HandleEvent(event.AgentEditProposed{Edit: event.PendingEdit{
		ID: "e5", Path: "", Search: "hello", Replace: "goodbye",
	}})
	if diff, _ := sess.ReviewEdit(); diff == nil {
		t.Fatalf("ReviewEdit returned nil diff")
	}
	if ok, reason := sess.ApproveEdit("hello", "goodbye"); !ok {
		t.Fatalf("ApproveEdit: %s", reason)
	}

	// Continue without any developer edits to the buffer.
	sess.Continue()

	kinds := sink.kinds()
	var contEv capture.Event
	for _, ev := range sink.snapshot() {
		if ev.Kind == "continue" {
			contEv = ev
		}
	}
	if contEv.Kind == "" {
		t.Fatalf("no continue event; kinds = %v", kinds)
	}
	if got := contEv.Payload["was_modified"]; got != false {
		t.Errorf("was_modified = %v, want false", got)
	}
	if _, has := contEv.Payload["proposed_content"]; has {
		t.Errorf("proposed_content present on unmodified continue: %+v", contEv.Payload)
	}
}

// TestCaptureContinueWithDevModification verifies that if the developer
// edits the buffer between approval and continue, the capture payload
// includes both the expected (post-approval) and actual (dev-edited)
// content so downstream consumers can reconstruct the refinement diff.
func TestCaptureContinueWithDevModification(t *testing.T) {
	t.Parallel()

	sess := newTestSession("hello world")
	sink := &fakeSink{}
	sess.SetEventSink(sink)

	sess.HandleEvent(event.AgentEditProposed{Edit: event.PendingEdit{
		ID: "e6", Path: "", Search: "hello", Replace: "goodbye",
	}})
	if diff, _ := sess.ReviewEdit(); diff == nil {
		t.Fatalf("ReviewEdit returned nil diff")
	}
	if ok, reason := sess.ApproveEdit("hello", "goodbye"); !ok {
		t.Fatalf("ApproveEdit: %s", reason)
	}

	// Developer edits the buffer between approval and continue.
	ed := sess.ActiveEditor()
	ed.Buf.Insert(0, ed.Buf.LineLen(0), " NEW")

	sess.Continue()

	var contEv capture.Event
	for _, ev := range sink.snapshot() {
		if ev.Kind == "continue" {
			contEv = ev
		}
	}
	if contEv.Kind == "" {
		t.Fatalf("no continue event captured")
	}
	if got := contEv.Payload["was_modified"]; got != true {
		t.Errorf("was_modified = %v, want true", got)
	}
	actual, _ := contEv.Payload["actual_content"].(string)
	if !strings.Contains(actual, "NEW") {
		t.Errorf("actual_content missing dev edit: %q", actual)
	}
	proposed, _ := contEv.Payload["proposed_content"].(string)
	if strings.Contains(proposed, "NEW") {
		t.Errorf("proposed_content should predate dev edit: %q", proposed)
	}
}

// TestCaptureAcceptedIncludesProposedReplaceWhenModified verifies that
// the accepted event records both the agent's original proposal and the
// developer's modified version when they differ. Essential for the
// preference-signal dataset the session log is designed to produce.
func TestCaptureAcceptedIncludesProposedReplaceWhenModified(t *testing.T) {
	t.Parallel()

	sess := newTestSession("hello world")
	sink := &fakeSink{}
	sess.SetEventSink(sink)

	sess.HandleEvent(event.AgentEditProposed{Edit: event.PendingEdit{
		ID: "e7", Path: "", Search: "hello", Replace: "agent-chose",
	}})
	if diff, _ := sess.ReviewEdit(); diff == nil {
		t.Fatalf("ReviewEdit returned nil diff")
	}
	// Developer modifies the replacement text in the diff overlay.
	if ok, reason := sess.ApproveEdit("hello", "dev-chose"); !ok {
		t.Fatalf("ApproveEdit: %s", reason)
	}

	var accEv capture.Event
	for _, ev := range sink.snapshot() {
		if ev.Kind == "accepted" {
			accEv = ev
		}
	}
	if accEv.Kind == "" {
		t.Fatalf("no accepted event captured")
	}
	if got := accEv.Payload["modified_by_user"]; got != true {
		t.Errorf("modified_by_user = %v, want true", got)
	}
	if got := accEv.Payload["proposed_replace"]; got != "agent-chose" {
		t.Errorf("proposed_replace = %v, want agent-chose", got)
	}
	if got := accEv.Payload["replace"]; got != "dev-chose" {
		t.Errorf("replace = %v, want dev-chose", got)
	}
}

// TestCaptureAppendErrorDoesNotPanic verifies that a sink returning an error
// does not crash the session — the error is logged (not asserted here) and
// the session continues. Matches the "never block the hot path" contract.
func TestCaptureAppendErrorDoesNotPanic(t *testing.T) {
	t.Parallel()

	sess := newTestSession("")
	sink := &fakeSink{err: errors.New("sink down")}
	sess.SetEventSink(sink)

	// Must not panic.
	sess.SubmitGoal("anything")
}
