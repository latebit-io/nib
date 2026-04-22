package demarkus

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/latebit-io/junto/engine/capture"
	"github.com/latebit-io/junto/engine/memory"
)

// fakeStore is an in-memory memory.Store for tests. Records every call so
// tests can assert on ordering, payloads, and version semantics.
type fakeStore struct {
	mu      sync.Mutex
	docs    map[string]memory.Document
	publish []publishCall
	appends []appendCall
	failPub error
	failApp error
}

type publishCall struct {
	path    string
	body    string
	version int
}

type appendCall struct {
	path    string
	body    string
	version int
}

func newFakeStore() *fakeStore {
	return &fakeStore{docs: make(map[string]memory.Document)}
}

func (f *fakeStore) Fetch(_ context.Context, p string) (memory.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.docs[p]
	if !ok {
		return memory.Document{}, memory.ErrNotFound
	}
	return d, nil
}

func (f *fakeStore) Publish(_ context.Context, p, body string, expected int) (memory.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publish = append(f.publish, publishCall{p, body, expected})
	if f.failPub != nil {
		return memory.Document{}, f.failPub
	}
	existing, ok := f.docs[p]
	if ok && expected != existing.Version {
		return memory.Document{}, memory.ErrConflict
	}
	if !ok && expected != 0 {
		return memory.Document{}, memory.ErrConflict
	}
	nextVersion := existing.Version + 1
	if !ok {
		nextVersion = 1
	}
	doc := memory.Document{Path: p, Body: body, Version: nextVersion}
	f.docs[p] = doc
	return doc, nil
}

func (f *fakeStore) Append(_ context.Context, p, body string, expected int) (memory.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appends = append(f.appends, appendCall{p, body, expected})
	if f.failApp != nil {
		return memory.Document{}, f.failApp
	}
	existing, ok := f.docs[p]
	if !ok {
		return memory.Document{}, memory.ErrNotFound
	}
	if expected != existing.Version {
		return memory.Document{}, memory.ErrConflict
	}
	existing.Body += body
	existing.Version++
	f.docs[p] = existing
	return existing, nil
}

func (f *fakeStore) List(context.Context, string) ([]string, error) { return nil, nil }

// snapshot returns the recorded publish/append calls and the body of
// the single tracked document. Every test in this file uses a unique
// session ID, so the fake never holds more than one document at a
// time; the guard panics if that assumption is violated so a future
// multi-doc test fails loudly instead of silently returning one doc's
// body. Callers that need per-path bodies should switch to a map.
func (f *fakeStore) snapshot() (pubs []publishCall, appends []appendCall, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pubs = append(pubs, f.publish...)
	appends = append(appends, f.appends...)
	if len(f.docs) > 1 {
		panic("fakeStore.snapshot: multiple docs tracked — update the test or return a map")
	}
	for _, d := range f.docs {
		body = d.Body
	}
	return
}

// flushSink closes the sink and waits for the dispatch goroutine to drain
// under a reasonable deadline. Failure to drain in time fails the test —
// the sink's Close contract guarantees completion before ctx expiry.
func flushSink(t *testing.T, s *Sink) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestFirstEventPublishesWithVersionZero verifies the initial write creates
// the document via Publish(expectedVersion=0) with a YAML front-matter
// header, and that subsequent events are appended with the incrementing
// version tracked locally.
func TestFirstEventPublishesWithVersionZero(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	sink := New(store, "test-session", Config{})

	_ = sink.Append(context.Background(), capture.Event{
		Kind: "intent", Payload: map[string]any{"goal": "fix bug"},
	})
	_ = sink.Append(context.Background(), capture.Event{
		Kind: "proposal", Payload: map[string]any{"id": "e1"},
	})
	flushSink(t, sink)

	pubs, apps, body := store.snapshot()
	if len(pubs) != 1 {
		t.Fatalf("publish calls = %d, want 1", len(pubs))
	}
	if pubs[0].version != 0 {
		t.Errorf("first publish expectedVersion = %d, want 0", pubs[0].version)
	}
	if !strings.Contains(pubs[0].body, "session_id: test-session") {
		t.Errorf("first publish body missing session_id header: %q", pubs[0].body)
	}
	// Two appends: the proposal event and the summary block written by Close.
	if len(apps) != 2 {
		t.Fatalf("append calls = %d, want 2 (event + close summary)", len(apps))
	}
	if apps[0].version != 1 {
		t.Errorf("first append expectedVersion = %d, want 1", apps[0].version)
	}
	if !strings.Contains(body, `"goal":"fix bug"`) {
		t.Errorf("document body missing intent payload: %q", body)
	}
	if !strings.Contains(body, "intent") || !strings.Contains(body, "proposal") {
		t.Errorf("document body missing kind headings: %q", body)
	}
}

// TestAppendNeverBlocks ensures the hot-path Append returns immediately
// even when the dispatch goroutine is blocked and the buffer is saturated.
// Saturation is achieved by configuring a tiny buffer and a slow store.
func TestAppendNeverBlocks(t *testing.T) {
	t.Parallel()

	// A store that blocks Publish forever so the dispatch goroutine
	// cannot drain the buffer.
	blocker := &blockingStore{gate: make(chan struct{})}
	sink := New(blocker, "blocked", Config{BufferSize: 1})
	defer func() {
		close(blocker.gate)
		_ = sink.Close(context.Background())
	}()

	// Buffer holds 1. First Append lands in buffer; second, third, fourth
	// all overflow because the dispatch goroutine is stuck inside Publish.
	// Every call must return without blocking.
	for i := range 4 {
		done := make(chan error, 1)
		go func() {
			done <- sink.Append(context.Background(), capture.Event{Kind: "x"})
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Append returned error: %v", err)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("Append blocked on call #%d", i)
		}
	}
	if sink.DropCount() == 0 {
		t.Errorf("DropCount = 0; expected overflow drops once buffer saturated")
	}
}

// blockingStore is a memory.Store whose Publish blocks until gate is closed.
// Used to reliably saturate the sink's buffer in TestAppendNeverBlocks.
type blockingStore struct{ gate chan struct{} }

func (b *blockingStore) Fetch(context.Context, string) (memory.Document, error) {
	<-b.gate
	return memory.Document{}, memory.ErrNotFound
}
func (b *blockingStore) Publish(context.Context, string, string, int) (memory.Document, error) {
	<-b.gate
	return memory.Document{}, errors.New("blocked")
}
func (b *blockingStore) Append(context.Context, string, string, int) (memory.Document, error) {
	<-b.gate
	return memory.Document{}, errors.New("blocked")
}
func (b *blockingStore) List(context.Context, string) ([]string, error) { return nil, nil }

// TestRedactorApplied verifies a configured Redactor is called before
// serialisation so sensitive fields can be masked at the wire boundary.
func TestRedactorApplied(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	sink := New(store, "redacted", Config{
		Redactor: func(e capture.Event) capture.Event {
			e.Payload = map[string]any{"goal": "REDACTED"}
			return e
		},
	})
	_ = sink.Append(context.Background(), capture.Event{
		Kind: "intent", Payload: map[string]any{"goal": "original secret"},
	})
	flushSink(t, sink)

	_, _, body := store.snapshot()
	if strings.Contains(body, "original secret") {
		t.Errorf("redactor not applied; body contains raw secret: %q", body)
	}
	if !strings.Contains(body, "REDACTED") {
		t.Errorf("redacted value missing from body: %q", body)
	}
}

// TestFieldSizeCap verifies large string values in Payload are truncated
// with a marker so a runaway buffer paste cannot balloon the session doc.
func TestFieldSizeCap(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	sink := New(store, "capped", Config{MaxFieldBytes: 16})

	big := strings.Repeat("A", 1024)
	_ = sink.Append(context.Background(), capture.Event{
		Kind: "proposal", Payload: map[string]any{"replace": big},
	})
	flushSink(t, sink)

	_, _, body := store.snapshot()
	if strings.Count(body, "A") >= 1024 {
		t.Errorf("field cap not applied; body contains full %d-byte field", 1024)
	}
	if !strings.Contains(body, "truncated") {
		t.Errorf("truncation marker missing from body: %q", body)
	}
}

// TestFieldSizeCapUTF8Safe verifies the rune-aware truncation. Byte-index
// slicing would split the 2-byte é rune (0xC3 0xA9) mid-sequence when
// maxBytes lands on the continuation byte; the cut must retreat to the
// preceding rune boundary so the persisted payload is valid UTF-8.
func TestFieldSizeCapUTF8Safe(t *testing.T) {
	t.Parallel()

	// Build a string where byte 5 is a continuation byte. "héllo世界"
	// layout: h(1) é(2) l(1) l(1) o(1) 世(3) 界(3).
	// Cap to 6 bytes: naive slice ends mid-"世", which is invalid UTF-8.
	store := newFakeStore()
	sink := New(store, "utf8-safe", Config{MaxFieldBytes: 6})

	_ = sink.Append(context.Background(), capture.Event{
		Kind: "proposal", Payload: map[string]any{"replace": "héllo世界"},
	})
	flushSink(t, sink)

	_, _, body := store.snapshot()

	// The persisted JSON must be valid UTF-8. Any mid-rune slice would
	// leave a stray continuation byte and fail this check.
	if !utf8.ValidString(body) {
		t.Errorf("persisted body is not valid UTF-8 — truncation split a rune: %q", body)
	}
	if !strings.Contains(body, "héllo") {
		t.Errorf("body missing the ASCII+é prefix that fits under the cap: %q", body)
	}
	// The 世 rune (3 bytes at offsets 6,7,8) cannot fit under maxBytes=6
	// once the é has consumed bytes 1–2, so it must not appear in the
	// truncated output.
	if strings.Contains(body, "世") {
		t.Errorf("truncated body includes a rune past the cap: %q", body)
	}
}

// TestAppendAfterCloseReturnsError verifies Close is a hard boundary —
// calls that arrive after it must surface an error so callers (usually
// tests) can detect misuse. The production session path never hits this
// because Close runs at shutdown.
func TestAppendAfterCloseReturnsError(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	sink := New(store, "closed-ok", Config{})
	flushSink(t, sink)

	err := sink.Append(context.Background(), capture.Event{Kind: "x"})
	if err == nil {
		t.Errorf("Append after Close returned nil; expected error")
	}
}

// TestCloseWritesSummary verifies Close appends a Session Summary block
// with per-kind event counts once the dispatch goroutine has drained.
// Gives operators a visible end-of-session marker without having to
// walk the entire document.
func TestCloseWritesSummary(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	sink := New(store, "summary-test", Config{})

	_ = sink.Append(context.Background(), capture.Event{Kind: "intent"})
	_ = sink.Append(context.Background(), capture.Event{Kind: "proposal"})
	_ = sink.Append(context.Background(), capture.Event{Kind: "proposal"})
	_ = sink.Append(context.Background(), capture.Event{Kind: "accepted"})
	flushSink(t, sink)

	_, _, body := store.snapshot()
	if !strings.Contains(body, "## Session Summary") {
		t.Fatalf("session summary header missing from body: %q", body)
	}
	if !strings.Contains(body, `"intent":1`) {
		t.Errorf("intent count missing from summary: %q", body)
	}
	if !strings.Contains(body, `"proposal":2`) {
		t.Errorf("proposal count missing from summary: %q", body)
	}
	if !strings.Contains(body, `"accepted":1`) {
		t.Errorf("accepted count missing from summary: %q", body)
	}
	if !strings.Contains(body, `"dropped":0`) {
		t.Errorf("dropped count missing from summary: %q", body)
	}
}

// TestCloseSkipsSummaryWhenNothingWritten verifies a Close on a sink
// that never successfully wrote an event does not emit a summary block —
// avoids a lonely "Session Summary" in a project that never ran an
// agent turn.
func TestCloseSkipsSummaryWhenNothingWritten(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	sink := New(store, "never-written", Config{})
	flushSink(t, sink)

	pubs, apps, _ := store.snapshot()
	if len(pubs) != 0 || len(apps) != 0 {
		t.Errorf("unexpected writes: publish=%d append=%d", len(pubs), len(apps))
	}
}

// TestAppendCloseNoPanicOrRace hammers Append and Close concurrently so
// the race detector (and the runtime's send-on-closed-channel panic)
// catch any regression in the Append/Close synchronisation. Without
// holding s.mu across the non-blocking select in Append, the sequence
// "Append observes closed=false → Close sets closed=true and closes
// the channel → Append sends" would panic on closed-channel send.
func TestAppendCloseNoPanicOrRace(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	sink := New(store, "race-close", Config{BufferSize: 4})

	var wg sync.WaitGroup

	// Spawn many senders so Append is overwhelmingly likely to be
	// mid-select when Close fires.
	for range 8 {
		wg.Go(func() {
			for range 200 {
				_ = sink.Append(context.Background(), capture.Event{Kind: "x"})
			}
		})
	}

	// Small head-start so appenders are already in flight before
	// Close runs.
	time.Sleep(time.Millisecond)
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sink.Close(closeCtx); err != nil {
		t.Errorf("Close returned: %v", err)
	}
	wg.Wait()
}

// TestCloseIdempotent verifies Close can safely be called more than once
// without panicking or leaking goroutines.
func TestCloseIdempotent(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	sink := New(store, "idempotent", Config{})
	flushSink(t, sink)
	// Second Close is a no-op.
	if err := sink.Close(context.Background()); err != nil {
		t.Errorf("second Close returned error: %v", err)
	}
}
