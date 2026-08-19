package headless

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/nib/kit/event"
)

// mockAgent implements [Agent] for tests. runFunc, when set, fires on
// Prompt to push events onto the supplied channel. replyCh is a
// one-shot signal that closes on the first Reply call so runFunc can
// deterministically wait for follow-up input without spin-waiting.
//
// runFunc bodies send bare (no select+ctx) — safe only because every
// script sends fewer events than the channel buffer (16), so a send can
// never block even if the runner stops draining. A runFunc that loops
// or sends unboundedly must switch to the ctx-cancellable select
// pattern (see cmd/agent/main_test.go's forwarder).
type mockAgent struct {
	mu sync.Mutex

	events    chan<- event.Event
	runFunc   func()
	cancelled bool
	replied   string
	prompted  string
	promptErr error
	replyOK   bool
	replyCh   chan struct{}
	replyOnce sync.Once
}

func newMockAgent(events chan<- event.Event) *mockAgent {
	return &mockAgent{
		events:  events,
		replyOK: true,
		replyCh: make(chan struct{}),
	}
}

func (m *mockAgent) Prompt(_ context.Context, prompt string) error {
	m.mu.Lock()
	m.prompted = prompt
	rf := m.runFunc
	err := m.promptErr
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if rf != nil {
		go rf()
	}
	return nil
}

func (m *mockAgent) Reply(_ context.Context, input string) bool {
	m.mu.Lock()
	m.replied = input
	ok := m.replyOK
	m.mu.Unlock()
	m.replyOnce.Do(func() { close(m.replyCh) })
	return ok
}

func (m *mockAgent) Cancel() {
	m.mu.Lock()
	m.cancelled = true
	m.mu.Unlock()
}

func (m *mockAgent) wasCancelled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancelled
}

func TestRunner_Run_TokensAndDone(t *testing.T) {
	events := make(chan event.Event, 16)
	mock := newMockAgent(events)
	mock.runFunc = func() {
		events <- event.AgentToken{Text: "Hello "}
		events <- event.AgentToken{Text: "world"}
		events <- event.AgentDone{Success: true}
	}

	r := New(mock, events, WithStderr(io.Discard))
	result := r.Run(context.Background(), "say hi")

	if !result.Success {
		t.Errorf("Success = false, want true")
	}
	if result.Summary != "Hello world" {
		t.Errorf("Summary = %q, want %q", result.Summary, "Hello world")
	}
	if mock.prompted != "say hi" {
		t.Errorf("prompted = %q, want %q", mock.prompted, "say hi")
	}
}

func TestRunner_Run_PromptError_ReturnsErrorResult(t *testing.T) {
	events := make(chan event.Event, 16)
	mock := newMockAgent(events)
	mock.promptErr = errors.New("agent closed")

	r := New(mock, events)
	result := r.Run(context.Background(), "x")

	if result.Success {
		t.Errorf("Success = true, want false on prompt error")
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "agent closed") {
		t.Errorf("Errors = %v, want contains 'agent closed'", result.Errors)
	}
}

func TestRunner_Drive_NoPromptCall(t *testing.T) {
	events := make(chan event.Event, 16)
	mock := newMockAgent(events)
	go func() {
		events <- event.AgentToken{Text: "hi"}
		events <- event.AgentDone{Success: true}
	}()

	r := New(mock, events)
	result := r.Drive(context.Background())

	if !result.Success {
		t.Errorf("Success = false, want true")
	}
	if mock.prompted != "" {
		t.Errorf("prompted = %q, want empty (Drive must not call Prompt)", mock.prompted)
	}
}

func TestRunner_TTY_StreamsTokensToStderr(t *testing.T) {
	events := make(chan event.Event, 16)
	mock := newMockAgent(events)
	mock.runFunc = func() {
		events <- event.AgentToken{Text: "live output"}
		events <- event.AgentDone{Success: true}
	}

	var stderr bytes.Buffer
	r := New(mock, events, WithStderr(&stderr), WithTTY(true))
	r.Run(context.Background(), "test")

	if !strings.Contains(stderr.String(), "live output") {
		t.Errorf("stderr = %q, want to contain 'live output'", stderr.String())
	}
}

func TestRunner_NonTTY_StderrSilent(t *testing.T) {
	events := make(chan event.Event, 16)
	mock := newMockAgent(events)
	mock.runFunc = func() {
		events <- event.AgentToken{Text: "noisy"}
		events <- event.AgentDone{Success: true}
	}

	var stderr bytes.Buffer
	r := New(mock, events, WithStderr(&stderr), WithTTY(false))
	r.Run(context.Background(), "test")

	if stderr.Len() != 0 {
		t.Errorf("stderr in non-TTY mode = %q, want empty", stderr.String())
	}
}

func TestRunner_AgentError_CollectedInResult(t *testing.T) {
	events := make(chan event.Event, 16)
	mock := newMockAgent(events)
	mock.runFunc = func() {
		events <- event.AgentError{Err: "LLM timeout"}
		events <- event.AgentDone{Success: false}
	}

	r := New(mock, events)
	result := r.Run(context.Background(), "fail")

	if result.Success {
		t.Errorf("Success = true, want false")
	}
	if len(result.Errors) != 1 || result.Errors[0] != "LLM timeout" {
		t.Errorf("Errors = %v, want [LLM timeout]", result.Errors)
	}
}

func TestRunner_AgentWaiting_SingleShot_CancelsAndExits(t *testing.T) {
	events := make(chan event.Event, 16)
	mock := newMockAgent(events)
	mock.runFunc = func() {
		events <- event.AgentToken{Text: "thought"}
		events <- event.AgentWaiting{}
	}

	r := New(mock, events)
	result := r.Run(context.Background(), "one shot")

	if !result.Success {
		t.Errorf("Success = false, want true (clean single-shot exit)")
	}
	if !mock.wasCancelled() {
		t.Errorf("agent.Cancel() not called in single-shot AgentWaiting")
	}
	if result.Summary != "thought" {
		t.Errorf("Summary = %q, want %q", result.Summary, "thought")
	}
}

// TestRunner_AgentWaiting_SingleShot_ErrorsMeanFailure: an agent that
// errors and then yields must not report Success.
func TestRunner_AgentWaiting_SingleShot_ErrorsMeanFailure(t *testing.T) {
	events := make(chan event.Event, 16)
	mock := newMockAgent(events)
	mock.runFunc = func() {
		events <- event.AgentError{Err: "tool exploded"}
		events <- event.AgentWaiting{}
	}

	r := New(mock, events)
	result := r.Run(context.Background(), "one shot")

	if result.Success {
		t.Errorf("Success = true, want false when Errors is non-empty")
	}
	if len(result.Errors) != 1 {
		t.Errorf("Errors = %v, want one entry", result.Errors)
	}
}

func TestRunner_AgentWaiting_TTY_RepliesFromStdin(t *testing.T) {
	events := make(chan event.Event, 16)
	mock := newMockAgent(events)
	stdin := strings.NewReader("follow up\n")

	mock.runFunc = func() {
		events <- event.AgentToken{Text: "first turn"}
		events <- event.AgentWaiting{}
		<-mock.replyCh // deterministic wait for the runner's Reply call
		events <- event.AgentDone{Success: true}
	}

	r := New(mock, events,
		WithStdin(stdin),
		WithStderr(io.Discard),
		WithTTY(true),
	)
	result := r.Run(context.Background(), "start")

	if !result.Success {
		t.Errorf("Success = false, want true")
	}
	if mock.replied != "follow up" {
		t.Errorf("replied = %q, want %q", mock.replied, "follow up")
	}
}

// TestRunner_AgentWaiting_TTY_BlankLine_RepromptsNotExits locks the
// REPL convention that a blank line is a no-op (re-prompt), not a
// termination signal. Pre-fix, an accidental enter on an empty line
// collapsed onto the same path as Ctrl+D and ended the session.
func TestRunner_AgentWaiting_TTY_BlankLine_RepromptsNotExits(t *testing.T) {
	events := make(chan event.Event, 16)
	mock := newMockAgent(events)
	stdin := strings.NewReader("\nactual reply\n")

	mock.runFunc = func() {
		events <- event.AgentWaiting{}
		<-mock.replyCh
		events <- event.AgentDone{Success: true}
	}

	r := New(mock, events,
		WithStdin(stdin),
		WithStderr(io.Discard),
		WithTTY(true),
	)
	result := r.Run(context.Background(), "start")

	if !result.Success {
		t.Errorf("Success = false, want true")
	}
	if mock.replied != "actual reply" {
		t.Errorf("replied = %q, want %q (blank line must re-prompt, not call Reply)", mock.replied, "actual reply")
	}
}

// TestRunner_AgentWaiting_TTY_CtxCancel_SurfacesErrorNotSuccess
// verifies that context cancellation while readInput is blocked on
// stdin produces a failed run with the cancellation surfaced —
// pre-fix, this collapsed onto the clean-EOF path and was reported
// as Success=true.
func TestRunner_AgentWaiting_TTY_CtxCancel_SurfacesErrorNotSuccess(t *testing.T) {
	events := make(chan event.Event, 16)
	mock := newMockAgent(events)

	// Pipe whose writer is never used: Read blocks indefinitely so
	// readInput sits in its select waiting for ctx.Done() instead
	// of getting a stdin line.
	stdinR, stdinW := io.Pipe()
	t.Cleanup(func() { _ = stdinW.Close() })

	ctx, cancel := context.WithCancel(context.Background())

	mock.runFunc = func() {
		events <- event.AgentWaiting{}
		// Give the runner a moment to reach handleWaiting →
		// readInput before cancelling. If scheduling slips and
		// the outer drain loop sees ctx.Done first, the
		// observable result is identical (Success=false + ctx
		// error appended), so the test stays correct either way.
		time.Sleep(20 * time.Millisecond)
		cancel()
	}

	r := New(mock, events,
		WithStdin(stdinR),
		WithStderr(io.Discard),
		WithTTY(true),
	)
	result := r.Run(ctx, "start")

	if result.Success {
		t.Errorf("Success = true, want false on ctx cancellation")
	}
	if !mock.wasCancelled() {
		t.Errorf("agent.Cancel() not called")
	}
	if len(result.Errors) == 0 || !strings.Contains(result.Errors[0], "cancel") {
		t.Errorf("Errors = %v, want one mentioning cancellation", result.Errors)
	}
}

// errReader is an io.Reader that always returns the configured error.
// Used to drive a scanner failure path through bufio.Scanner.
type errReader struct{ err error }

func (r *errReader) Read(_ []byte) (int, error) { return 0, r.err }

// TestRunner_AgentWaiting_TTY_ScannerError_SurfacesError verifies
// that a scanner failure during readInput is reported as a failed
// run with the underlying error attached, not as a clean EOF
// (pre-fix, the goroutine slog.Error'd and the runner reported
// Success=true).
func TestRunner_AgentWaiting_TTY_ScannerError_SurfacesError(t *testing.T) {
	events := make(chan event.Event, 16)
	mock := newMockAgent(events)
	stdin := &errReader{err: errors.New("disk fail")}

	mock.runFunc = func() {
		events <- event.AgentWaiting{}
	}

	r := New(mock, events,
		WithStdin(stdin),
		WithStderr(io.Discard),
		WithTTY(true),
	)
	result := r.Run(context.Background(), "start")

	if result.Success {
		t.Errorf("Success = true, want false on scanner error")
	}
	if !mock.wasCancelled() {
		t.Errorf("agent.Cancel() not called")
	}
	if len(result.Errors) == 0 || !strings.Contains(result.Errors[0], "disk fail") {
		t.Errorf("Errors = %v, want one mentioning 'disk fail'", result.Errors)
	}
}

func TestRunner_AgentWaiting_TTY_StdinEOF_CancelsAndExits(t *testing.T) {
	events := make(chan event.Event, 16)
	mock := newMockAgent(events)
	stdin := strings.NewReader("") // immediate EOF

	mock.runFunc = func() {
		events <- event.AgentWaiting{}
	}

	r := New(mock, events,
		WithStdin(stdin),
		WithStderr(io.Discard),
		WithTTY(true),
	)
	result := r.Run(context.Background(), "start")

	if !result.Success {
		t.Errorf("Success = false, want true on clean EOF")
	}
	if !mock.wasCancelled() {
		t.Errorf("agent.Cancel() not called on stdin EOF")
	}
}

func TestRunner_ContextCancelled(t *testing.T) {
	events := make(chan event.Event, 16)
	mock := newMockAgent(events)

	ctx, cancel := context.WithCancel(context.Background())
	mock.runFunc = func() { cancel() }

	r := New(mock, events)
	result := r.Run(ctx, "will cancel")

	if result.Success {
		t.Errorf("Success = true, want false on ctx cancellation")
	}
	if len(result.Errors) == 0 || !strings.Contains(result.Errors[0], "cancel") {
		t.Errorf("Errors = %v, want one mentioning 'cancel'", result.Errors)
	}
	if !mock.wasCancelled() {
		t.Errorf("agent.Cancel() not called on ctx cancellation")
	}
}

// customEvent is a domain-specific event used to verify EventHandler
// dispatch for unknown event types.
type customEvent struct{ payload string }

func (customEvent) Event() {}

func TestRunner_EventHandler_DispatchesUnknown(t *testing.T) {
	events := make(chan event.Event, 16)
	mock := newMockAgent(events)

	var seen []event.Event
	handler := func(_ context.Context, ev event.Event) error {
		seen = append(seen, ev)
		return nil
	}

	mock.runFunc = func() {
		events <- customEvent{payload: "hello"}
		events <- event.AgentDone{Success: true}
	}

	r := New(mock, events, WithHandler(handler))
	result := r.Run(context.Background(), "x")

	if !result.Success {
		t.Errorf("Success = false")
	}
	if len(seen) != 1 {
		t.Fatalf("handler called %d times, want 1", len(seen))
	}
	if e, ok := seen[0].(customEvent); !ok || e.payload != "hello" {
		t.Errorf("handler received %+v, want customEvent{hello}", seen[0])
	}
}

func TestRunner_EventHandler_ErrorAppendsToResult(t *testing.T) {
	events := make(chan event.Event, 16)
	mock := newMockAgent(events)

	handler := func(_ context.Context, _ event.Event) error {
		return errors.New("handler boom")
	}

	mock.runFunc = func() {
		events <- customEvent{}
		events <- event.AgentDone{Success: true}
	}

	r := New(mock, events, WithHandler(handler))
	result := r.Run(context.Background(), "x")

	// Run still completes; handler error becomes a non-fatal entry.
	if !result.Success {
		t.Errorf("Success = false, want true (handler error is non-fatal)")
	}
	if len(result.Errors) != 1 || result.Errors[0] != "handler boom" {
		t.Errorf("Errors = %v, want [handler boom]", result.Errors)
	}
}

// customStatus is a domain-specific status kind used to verify that
// AgentStatus events with unknown kinds are forwarded to the handler.
const customStatus event.StatusKind = "custom-domain"

func TestRunner_AgentStatus_UnknownKind_ForwardedToHandler(t *testing.T) {
	events := make(chan event.Event, 16)
	mock := newMockAgent(events)

	var seenKind event.StatusKind
	handler := func(_ context.Context, ev event.Event) error {
		if s, ok := ev.(event.AgentStatus); ok {
			seenKind = s.Status
		}
		return nil
	}

	mock.runFunc = func() {
		events <- event.AgentStatus{Status: customStatus}
		events <- event.AgentDone{Success: true}
	}

	r := New(mock, events, WithHandler(handler))
	r.Run(context.Background(), "x")

	if seenKind != customStatus {
		t.Errorf("handler received status %q, want %q", seenKind, customStatus)
	}
}

func TestRunner_AgentStatus_GenericKind_NotForwardedToHandler(t *testing.T) {
	events := make(chan event.Event, 16)
	mock := newMockAgent(events)

	var handlerCalls int
	handler := func(_ context.Context, _ event.Event) error {
		handlerCalls++
		return nil
	}

	// Every kit-generic StatusKind must be owned by handleStatus —
	// none should reach the handler. Adding a new generic kind to
	// kit/event without extending handleStatus would surface here.
	mock.runFunc = func() {
		events <- event.AgentStatus{Status: event.StatusThinking}
		events <- event.AgentStatus{Status: event.StatusPlanning}
		events <- event.AgentStatus{Status: event.StatusIdle}
		events <- event.AgentStatus{Status: event.StatusWaiting}
		events <- event.AgentStatus{Status: event.StatusFinished}
		events <- event.AgentStatus{Status: event.StatusPlanningWaiting}
		events <- event.AgentDone{Success: true}
	}

	r := New(mock, events, WithHandler(handler))
	r.Run(context.Background(), "x")

	if handlerCalls != 0 {
		t.Errorf("handler called %d times for generic statuses, want 0", handlerCalls)
	}
}

func TestRunner_EventsChannelClosed_PreservesPartialSummary(t *testing.T) {
	events := make(chan event.Event, 4)
	mock := newMockAgent(events)
	mock.runFunc = func() {
		events <- event.AgentToken{Text: "partial"}
		close(events)
	}

	r := New(mock, events)
	result := r.Run(context.Background(), "x")

	// Closed channel without AgentDone leaves Success at its zero
	// value; the partial transcript must still surface so callers
	// see what arrived before the producer terminated.
	if result.Success {
		t.Errorf("Success = true, want false (no AgentDone before close)")
	}
	if result.Summary != "partial" {
		t.Errorf("Summary = %q, want %q (partial summary must be preserved on close)", result.Summary, "partial")
	}
}

// driveOnlyAgent satisfies [Agent] but NOT [Prompter] — used to
// verify Run rejects agents whose run-start API is richer than
// (ctx, prompt).
type driveOnlyAgent struct {
	cancelled bool
}

func (a *driveOnlyAgent) Reply(_ context.Context, _ string) bool { return true }
func (a *driveOnlyAgent) Cancel()                                { a.cancelled = true }

func TestRunner_Run_NonPrompter_ReturnsErrorResult(t *testing.T) {
	events := make(chan event.Event, 4)
	a := &driveOnlyAgent{}

	r := New(a, events)
	result := r.Run(context.Background(), "x")

	if result.Success {
		t.Errorf("Success = true, want false")
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "Prompter") {
		t.Errorf("Errors = %v, want one mentioning 'Prompter'", result.Errors)
	}
}

func TestResult_WriteJSON(t *testing.T) {
	r := &Result{Success: true, Summary: "done"}
	var buf bytes.Buffer
	if err := r.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var decoded Result
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !decoded.Success || decoded.Summary != "done" {
		t.Errorf("decoded = %+v, want Success=true Summary=done", decoded)
	}
}
