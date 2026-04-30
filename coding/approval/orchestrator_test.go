package approval

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/junto/coding/tools"
	"github.com/latebit-io/junto/engine/event"
)

// stubContextSet is a minimal ContextSet implementation for tests.
// Records what was added so tests can assert on AddContext calls.
type stubContextSet struct {
	mu      sync.Mutex
	members map[string]bool
}

func newStubContextSet(initial ...string) *stubContextSet {
	cs := &stubContextSet{members: make(map[string]bool)}
	for _, p := range initial {
		cs.members[p] = true
	}
	return cs
}

func (c *stubContextSet) InContext(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.members[path]
}

func (c *stubContextSet) AddContext(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.members[path] = true
}

// orchTestRig bundles the moving parts of an Orchestrator test so
// each case stays compact. The Send and SendCritical wrappers both
// record into the same events slice — that's the only way assertions
// against critical events (e.g. AgentEditProposed) can fire, since
// the orchestrator routes the proposal event through SendCritical
// rather than Send. proposalDelivered is closed (exactly once) when
// AgentEditProposed is delivered, giving goroutine-driven tests a
// deterministic rendezvous point in place of racy time.Sleep.
//
// validateFn / autonomousFn / sendCriticalFn can be overridden per
// test; recordedEdits captures RecordEdit invocations.
type orchTestRig struct {
	cache                 *tools.FileCache
	ctxSet                *stubContextSet
	events                []event.Event
	eventsMu              sync.Mutex
	recordedEdits         []tools.EditProposal
	validateFn            func(ctx context.Context, p tools.EditProposal) ([]event.ValidatorSummary, string)
	autonomousFn          func() bool
	sendCriticalFn        func(ctx context.Context, ev event.Event) error
	proposalDelivered     chan struct{}
	proposalDeliveredOnce sync.Once
}

func newRig() *orchTestRig {
	return &orchTestRig{
		cache:        tools.NewFileCache(),
		ctxSet:       newStubContextSet(),
		validateFn:   func(context.Context, tools.EditProposal) ([]event.ValidatorSummary, string) { return nil, "" },
		autonomousFn: func() bool { return false },
		sendCriticalFn: func(_ context.Context, _ event.Event) error {
			return nil
		},
		proposalDelivered: make(chan struct{}),
	}
}

func (r *orchTestRig) deps() Deps {
	return Deps{
		Cache:        r.cache,
		Workspace:    r.ctxSet,
		Send:         r.recordEvent,
		SendCritical: r.sendCriticalDelivery,
		Validate: func(ctx context.Context, p tools.EditProposal) ([]event.ValidatorSummary, string) {
			return r.validateFn(ctx, p)
		},
		RecordEdit: r.recordEdit,
		Autonomous: func() bool { return r.autonomousFn() },
		DiagDelay:  0, // skip the post-Continue diagnostic sleep in tests
	}
}

// sendCriticalDelivery is the rig's SendCritical wrapper. It calls
// the test-overridable [orchTestRig.sendCriticalFn] (so tests can
// inject a delivery failure), and on successful delivery records
// the event AND signals proposalDelivered for any AgentEditProposed
// event so goroutine-driven tests can rendezvous on actual delivery
// instead of a wall-clock sleep.
func (r *orchTestRig) sendCriticalDelivery(ctx context.Context, ev event.Event) error {
	if err := r.sendCriticalFn(ctx, ev); err != nil {
		return err
	}
	r.recordEvent(ev)
	if _, ok := ev.(event.AgentEditProposed); ok {
		r.proposalDeliveredOnce.Do(func() {
			close(r.proposalDelivered)
		})
	}
	return nil
}

func (r *orchTestRig) recordEvent(ev event.Event) {
	r.eventsMu.Lock()
	defer r.eventsMu.Unlock()
	r.events = append(r.events, ev)
}

func (r *orchTestRig) recordEdit(p tools.EditProposal) {
	r.recordedEdits = append(r.recordedEdits, p)
}

func (r *orchTestRig) snapshotEvents() []event.Event {
	r.eventsMu.Lock()
	defer r.eventsMu.Unlock()
	return append([]event.Event(nil), r.events...)
}

// sampleProposal is a stable EditProposal value used across tests.
func sampleProposal() tools.EditProposal {
	return tools.EditProposal{
		Edit: event.PendingEdit{
			ID:      "edit-1",
			Path:    "main.go",
			Search:  "old",
			Replace: "new",
		},
		Path:            "main.go",
		CanonPath:       "/proj/main.go",
		ExpectedContent: "package main\n\nfunc main() { _ = new }\n",
	}
}

// TestNewOrchestrator_PanicsOnMissingDeps locks the load-bearing-
// nil-callback contract: rather than silently producing wrong
// behaviour at runtime, NewOrchestrator panics at construction time
// when a required dependency is missing.
func TestNewOrchestrator_PanicsOnMissingDeps(t *testing.T) {
	t.Parallel()
	full := newRig().deps()
	cases := []struct {
		name   string
		mutate func(*Deps)
	}{
		{"nil Cache", func(d *Deps) { d.Cache = nil }},
		{"nil Workspace", func(d *Deps) { d.Workspace = nil }},
		{"nil Send", func(d *Deps) { d.Send = nil }},
		{"nil SendCritical", func(d *Deps) { d.SendCritical = nil }},
		{"nil Validate", func(d *Deps) { d.Validate = nil }},
		{"nil RecordEdit", func(d *Deps) { d.RecordEdit = nil }},
		{"nil Autonomous", func(d *Deps) { d.Autonomous = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := full
			tc.mutate(&d)
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("expected panic for %s", tc.name)
				}
			}()
			NewOrchestrator(d)
		})
	}
}

// TestHandle_ValidationShortCircuit verifies that a non-empty
// retryFeedback from the Validate callback short-circuits the flow:
// no AgentEditProposed event is sent, no approval is awaited, and
// the feedback string is returned verbatim as the body.
func TestHandle_ValidationShortCircuit(t *testing.T) {
	t.Parallel()
	r := newRig()
	r.validateFn = func(context.Context, tools.EditProposal) ([]event.ValidatorSummary, string) {
		return nil, "retry: shorten the search text"
	}
	o := NewOrchestrator(r.deps())

	body, isError := o.Handle(context.Background(), New(), sampleProposal())

	if isError {
		t.Errorf("validation retry should not be a tool error, got isError=true")
	}
	if body != "retry: shorten the search text" {
		t.Errorf("body = %q, want %q", body, "retry: shorten the search text")
	}
	for _, ev := range r.snapshotEvents() {
		if _, ok := ev.(event.AgentEditProposed); ok {
			t.Errorf("EditProposed event emitted on validation retry: %+v", ev)
		}
	}
	if len(r.recordedEdits) != 0 {
		t.Errorf("RecordEdit invoked on retry path: %d edits recorded", len(r.recordedEdits))
	}
}

// TestHandle_HappyPath_ApproveAndContinue covers the full success
// flow: validation passes, EditProposed is delivered, Approve fires,
// Continue delivers identical content, the file is added to the
// context set, and RecordEdit is invoked exactly once.
func TestHandle_HappyPath_ApproveAndContinue(t *testing.T) {
	t.Parallel()
	r := newRig()
	o := NewOrchestrator(r.deps())
	coord := New()
	p := sampleProposal()

	go func() {
		// Block until the orchestrator has actually delivered the
		// proposal event — i.e. it's about to park (or has just
		// parked) in AwaitApproval. coord.Continue is buffered, so
		// firing both signals back-to-back is safe: Approve
		// releases AwaitApproval, Continue queues for the
		// AwaitContinue that follows.
		<-r.proposalDelivered
		coord.Approve()
		coord.Continue(p.ExpectedContent)
	}()

	body, isError := o.Handle(context.Background(), coord, p)
	if isError {
		t.Errorf("happy path should not be a tool error, body=%q", body)
	}
	if !strings.Contains(body, "Edit applied successfully") {
		t.Errorf("body should report success, got %q", body)
	}
	if !r.ctxSet.InContext(p.Path) {
		t.Errorf("AddContext not invoked for %q", p.Path)
	}
	if len(r.recordedEdits) != 1 || r.recordedEdits[0].CanonPath != p.CanonPath {
		t.Errorf("RecordEdit not invoked exactly once for the proposal: %+v", r.recordedEdits)
	}
}

// TestHandle_DeveloperModifiedContinue surfaces the diff in the body
// and notes the LLM should recalibrate. The cache is updated to the
// new content so subsequent tool calls see the post-edit state.
func TestHandle_DeveloperModifiedContinue(t *testing.T) {
	t.Parallel()
	r := newRig()
	o := NewOrchestrator(r.deps())
	coord := New()
	p := sampleProposal()
	developerEdited := p.ExpectedContent + "\n// developer added this line\n"

	go func() {
		<-r.proposalDelivered
		coord.Approve()
		coord.Continue(developerEdited)
	}()

	body, _ := o.Handle(context.Background(), coord, p)
	if !strings.Contains(body, "developer modified your edit") {
		t.Errorf("body should flag developer modification, got %q", body)
	}
	cached, ok := r.cache.Get(p.CanonPath)
	if !ok || cached != developerEdited {
		t.Errorf("cache not updated to developer-edited content: ok=%v cached=%q", ok, cached)
	}
}

// TestHandle_Reject covers the rejection path: Reject is signaled,
// the body carries the rejection message with current cache content,
// no AddContext call, no RecordEdit.
//
// The cache is deliberately poisoned with a string that — under the
// older marker-scan implementation of [Handle] — would have been
// matched as a fatal error and triggered isError=true on what is
// actually a normal rejection. Locking this case prevents
// regression to the body-string-scan approach: a file's content is
// arbitrary developer text and may contain ANY substring including
// the literal "Error: agent canceled" or "Error: approval channel
// closed". The typed outcome carried by the inner handle is what
// drives isError now, so cache content cannot mislead it.
func TestHandle_Reject(t *testing.T) {
	t.Parallel()
	r := newRig()
	const poisonedContent = "package main\n// notes:\n//   Error: agent canceled — the test wrote this on purpose\n//   Error: approval channel closed — also on purpose\n// pre-edit content\n"
	r.cache.Set("/proj/main.go", poisonedContent)
	o := NewOrchestrator(r.deps())
	coord := New()
	p := sampleProposal()

	go func() {
		<-r.proposalDelivered
		coord.Reject()
	}()

	body, isError := o.Handle(context.Background(), coord, p)
	if isError {
		t.Errorf("reject is normal flow, not a tool error (poisoned cache content must not trigger isError); body=%q", body)
	}
	if !strings.Contains(body, "rejected this edit") {
		t.Errorf("body should report rejection, got %q", body)
	}
	if !strings.Contains(body, "pre-edit content") {
		t.Errorf("body should include current cache content, got %q", body)
	}
	if !strings.Contains(body, "Error: agent canceled") {
		t.Errorf("body should include the poisoned cache substring verbatim "+
			"(proves we are testing what we think we are testing); got %q", body)
	}
	if r.ctxSet.InContext(p.Path) {
		t.Errorf("AddContext should NOT be invoked on reject")
	}
	if len(r.recordedEdits) != 0 {
		t.Errorf("RecordEdit should NOT be invoked on reject: %d", len(r.recordedEdits))
	}
}

// TestHandle_CtxCanceled_DuringApproval returns the canceled marker
// (which Handle then surfaces as a tool error via the marker scan).
func TestHandle_CtxCanceled_DuringApproval(t *testing.T) {
	t.Parallel()
	r := newRig()
	o := NewOrchestrator(r.deps())
	coord := New()
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		// Wait for proposal delivery so we exercise the
		// AwaitApproval cancel path specifically — a cancel that
		// fired before SendCritical would test a different return
		// path inside handle().
		<-r.proposalDelivered
		cancel()
	}()

	body, isError := o.Handle(ctx, coord, sampleProposal())
	if !isError {
		t.Errorf("ctx cancel should produce an isError result; body=%q", body)
	}
	if !strings.Contains(body, "Error: agent canceled") {
		t.Errorf("body = %q, want substring 'Error: agent canceled'", body)
	}
}

// TestHandle_SendCriticalFailure surfaces the delivery error as a
// tool error. The orchestrator must NOT block waiting for an
// approval signal that no one is going to send.
func TestHandle_SendCriticalFailure(t *testing.T) {
	t.Parallel()
	r := newRig()
	r.sendCriticalFn = func(_ context.Context, _ event.Event) error {
		return errors.New("frontend not draining events")
	}
	o := NewOrchestrator(r.deps())

	done := make(chan struct{})
	var body string
	var isError bool
	go func() {
		body, isError = o.Handle(context.Background(), New(), sampleProposal())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Handle blocked when SendCritical failed — should fail fast")
	}
	if !isError {
		t.Errorf("delivery failure should produce isError=true; body=%q", body)
	}
	if !strings.Contains(body, "could not deliver edit proposal") {
		t.Errorf("body = %q, want substring 'could not deliver edit proposal'", body)
	}
}

// TestHandle_AutonomousMode_SuppressesContinueHint asserts the
// "press Ctrl+N to continue" banner is rendered ONLY when the
// Autonomous callback returns false. At higher autonomy levels the
// continue fires automatically and the message is misleading visual
// noise.
func TestHandle_AutonomousMode_SuppressesContinueHint(t *testing.T) {
	t.Parallel()
	for _, autonomous := range []bool{false, true} {
		t.Run(map[bool]string{false: "guided", true: "autonomous"}[autonomous], func(t *testing.T) {
			t.Parallel()
			r := newRig()
			r.autonomousFn = func() bool { return autonomous }
			o := NewOrchestrator(r.deps())
			coord := New()
			p := sampleProposal()

			go func() {
				<-r.proposalDelivered
				coord.Approve()
				coord.Continue(p.ExpectedContent)
			}()

			_, _ = o.Handle(context.Background(), coord, p)

			var sawHint bool
			for _, ev := range r.snapshotEvents() {
				if tok, ok := ev.(event.AgentToken); ok && strings.Contains(tok.Text, "waiting for continue") {
					sawHint = true
					break
				}
			}
			if autonomous && sawHint {
				t.Errorf("autonomous mode emitted 'waiting for continue' hint")
			}
			if !autonomous && !sawHint {
				t.Errorf("guided mode did NOT emit 'waiting for continue' hint")
			}
		})
	}
}
