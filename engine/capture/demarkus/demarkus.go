// Package demarkus implements capture.SessionEventSink backed by a Mark
// Protocol document. Events are serialised as append-only markdown entries
// under a single per-process document so the session log is cheap to stream
// and easy to grep.
//
// The adapter is intentionally decoupled from any specific Mark server
// implementation — it only depends on the [memory.Store] interface, which
// the composition root satisfies with the active mcpadapter.
package demarkus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/latebit-io/nib/engine/capture"
	"github.com/latebit-io/nib/engine/memory"
)

// DefaultBufferSize is the number of pending events the sink buffers before
// dropping on overflow. Chosen to absorb a typical agent turn (goal +
// several proposals + accepts) without blocking; the drop path logs a warn
// so a persistently full buffer becomes visible in debug logs.
const DefaultBufferSize = 64

// DefaultMaxFieldBytes caps the size of any single payload string to prevent
// a runaway edit (e.g. a 10 MB buffer paste) from bloating the session doc.
// 8 KB matches the codebase's existing maxContentPreview constant for symmetry
// with the tools pipeline.
const DefaultMaxFieldBytes = 8 * 1024

// Config tunes the sink. Zero values fall back to the exported defaults.
type Config struct {
	// BufferSize is the maximum number of in-flight events queued between
	// Append and the dispatch goroutine. Overflow drops with a warn log.
	BufferSize int

	// MaxFieldBytes caps every string field in Event.Payload before
	// serialisation. Zero uses DefaultMaxFieldBytes; negative disables.
	MaxFieldBytes int

	// Redactor is applied to every event before serialisation. nil is
	// treated as identity.
	Redactor capture.Redactor

	// DocRoot is the Mark path prefix used for session documents.
	// Defaults to "/nib/sessions".
	DocRoot string

	// Clock provides the current time; overridable for tests. nil uses
	// time.Now.
	Clock func() time.Time
}

// Sink is a capture.SessionEventSink that streams events into a single
// Mark document. Safe for concurrent Append calls from multiple goroutines.
type Sink struct {
	store     memory.Store
	sessionID string
	path      string
	cfg       Config

	events chan capture.Event
	done   chan struct{} // closed when dispatch goroutine exits

	mu        sync.Mutex
	closed    bool
	version   int            // current server version of the session doc
	created   bool           // true once the initial Publish has landed
	dropCount int            // overflow drops since construction (monotonic)
	kindCount map[string]int // per-kind event tallies for the close summary
}

// New returns a Sink that writes session events to the given store under
// mark://{DocRoot}/{sessionID}.md. The sessionID is opaque from the sink's
// perspective; callers typically pass [session.Session.SessionID()].
//
// New starts a background goroutine that drains the event channel; call
// Close to stop it and flush pending events.
func New(store memory.Store, sessionID string, cfg Config) *Sink {
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = DefaultBufferSize
	}
	if cfg.MaxFieldBytes == 0 {
		cfg.MaxFieldBytes = DefaultMaxFieldBytes
	}
	if cfg.DocRoot == "" {
		cfg.DocRoot = "/nib/sessions"
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	s := &Sink{
		store:     store,
		sessionID: sessionID,
		path:      strings.TrimRight(cfg.DocRoot, "/") + "/" + sessionID + ".md",
		cfg:       cfg,
		events:    make(chan capture.Event, cfg.BufferSize),
		done:      make(chan struct{}),
		kindCount: make(map[string]int),
	}
	go s.run()
	return s
}

// Path returns the full Mark document path where this sink writes.
// Exposed primarily for diagnostics and tests.
func (s *Sink) Path() string { return s.path }

// DropCount returns the number of events dropped due to buffer overflow
// since the sink was constructed. Thread-safe.
func (s *Sink) DropCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropCount
}

// Append enqueues an event for asynchronous persistence. Never blocks the
// caller — if the internal buffer is full, the event is dropped and a warn
// log is emitted. Returns nil even on drop; the drop count is observable
// via [Sink.DropCount] for tests and diagnostics.
//
// The lock is held across the non-blocking select so Close cannot race
// between the closed-check and the channel send. Close also serialises on
// s.mu before calling close(s.events), so any Append that observes
// closed=false completes its send (or hits default) before Close can
// proceed to close the channel.
//
// Payload snapshotting: the redactor and size-cap transforms run here
// (not in the dispatch goroutine) so the enqueued event's Payload tree
// is already a fresh deep-copy by the time it reaches the channel. This
// closes the window where a caller could mutate Event.Payload between
// Append and persist and race the dispatch goroutine. Net cost is ~zero
// because capEventFields was already deep-copying; this moves the work
// earlier in the pipeline, not adds new allocations.
func (s *Sink) Append(_ context.Context, e capture.Event) error {
	// Transform before the lock so the synchronous critical section
	// stays short. Callers observe the canonical form immediately; the
	// dispatch goroutine reads from a snapshot that no other goroutine
	// holds a reference to.
	if s.cfg.Redactor != nil {
		e = s.cfg.Redactor(e)
	}
	if e.Timestamp.IsZero() {
		// Stamp after the redactor so a redactor that reconstructs the
		// event (and zeroes Timestamp) cannot reintroduce an unstamped
		// event into the queue. formatEvent fails closed on a zero
		// timestamp, so the invariant must hold here.
		e.Timestamp = s.cfg.Clock()
	}
	e = capEventFields(e, s.cfg.MaxFieldBytes)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("demarkus sink: append after close")
	}
	select {
	case s.events <- e:
		return nil
	default:
		s.dropCount++
		dropped := s.dropCount
		slog.Warn("demarkus capture: event dropped (buffer full)",
			"kind", e.Kind, "session", s.sessionID, "total_dropped", dropped)
		return nil
	}
}

// Close signals the dispatch goroutine to drain and stop, then appends
// a Session Summary block with per-kind event counts. Waits up to the
// deadline on ctx for the drain to complete; if the context fires first,
// the remaining events are abandoned and no summary is written.
func (s *Sink) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	close(s.events)
	select {
	case <-s.done:
		s.writeSummary(ctx)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run drains the event channel and persists each event. Exits when the
// channel is closed and empty.
func (s *Sink) run() {
	defer close(s.done)
	for e := range s.events {
		s.persist(e)
	}
}

// persist writes a single event to the Mark document. The first event
// publishes the document with a YAML front-matter header; subsequent
// events append markdown entries. Errors are logged and the sink
// continues — capture must not block the session.
//
// The event is already redacted and size-capped by Append, so persist
// just serialises + writes. Keeping transforms out of this goroutine
// means the dispatch loop has no synchronisation dependency on caller
// code paths (redactors, etc.) that might mutate shared state.
func (s *Sink) persist(e capture.Event) {
	body, err := formatEvent(e)
	if err != nil {
		slog.Warn("demarkus capture: format failed", "kind", e.Kind, "err", err)
		return
	}

	// Use a fresh context per write so a cancelled appCtx on shutdown
	// does not prevent final drain. The dispatch goroutine itself exits
	// when the events channel closes; Close enforces overall lifetime.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s.mu.Lock()
	created := s.created
	version := s.version
	s.mu.Unlock()

	var doc memory.Document
	if !created {
		header := formatHeader(s.sessionID, s.cfg.Clock())
		doc, err = s.store.Publish(ctx, s.path, header+body, 0)
		if errors.Is(err, memory.ErrConflict) {
			// Doc already exists (maybe from a crashed prior process);
			// fetch and append instead.
			existing, fetchErr := s.store.Fetch(ctx, s.path)
			if fetchErr != nil {
				slog.Warn("demarkus capture: publish conflict + fetch failed",
					"path", s.path, "err", fetchErr)
				return
			}
			doc, err = s.store.Append(ctx, s.path, body, existing.Version)
		}
	} else {
		doc, err = s.store.Append(ctx, s.path, body, version)
	}
	if err != nil {
		slog.Warn("demarkus capture: write failed",
			"path", s.path, "kind", e.Kind, "err", err)
		return
	}

	s.mu.Lock()
	s.created = true
	s.version = doc.Version
	s.kindCount[e.Kind]++
	s.mu.Unlock()
}

// writeSummary appends a closing "Session Summary" block with per-kind
// event counts. Called from Close after the event channel has drained.
// Best-effort: errors are logged and swallowed because the caller is in
// the shutdown path and cannot meaningfully react to a summary write
// failure.
func (s *Sink) writeSummary(ctx context.Context) {
	s.mu.Lock()
	if !s.created {
		// No events were ever written; nothing to summarise.
		s.mu.Unlock()
		return
	}
	counts := maps.Clone(s.kindCount)
	dropped := s.dropCount
	version := s.version
	s.mu.Unlock()

	body := formatSummary(counts, dropped, s.cfg.Clock())
	doc, err := s.store.Append(ctx, s.path, body, version)
	if err != nil {
		slog.Warn("demarkus capture: summary write failed",
			"path", s.path, "err", err)
		return
	}
	s.mu.Lock()
	s.version = doc.Version
	s.mu.Unlock()
}

// formatSummary renders the close-time summary block. encoding/json
// sorts map keys lexicographically when marshalling map[string]T, so
// the resulting "counts" object is deterministic across runs without
// hand-rolled iteration.
func formatSummary(counts map[string]int, dropped int, closedAt time.Time) string {
	payload := struct {
		ClosedAt string         `json:"closed_at"`
		Dropped  int            `json:"dropped"`
		Counts   map[string]int `json:"counts"`
	}{
		ClosedAt: closedAt.UTC().Format(time.RFC3339),
		Dropped:  dropped,
		Counts:   counts,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		// Cannot occur for the primitive shape above; log defensively
		// so a future schema change cannot fail silently.
		slog.Warn("demarkus capture: summary marshal failed", "err", err)
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Session Summary\n\n")
	b.WriteString("```json\n")
	b.Write(body)
	b.WriteString("\n```\n")
	return b.String()
}

// formatHeader returns the YAML front-matter block written once per document.
func formatHeader(sessionID string, started time.Time) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("session_id: ")
	b.WriteString(sessionID)
	b.WriteString("\n")
	b.WriteString("started: ")
	b.WriteString(started.UTC().Format(time.RFC3339))
	b.WriteString("\n---\n\n# nib Session\n")
	return b.String()
}

// formatEvent renders a single event as a markdown entry. Each entry is
// wall-clock-timestamped, tagged with the event kind, and followed by a
// fenced JSON payload so the document is trivially machine-parseable while
// still being readable.
//
// The timestamp must be pre-populated (Append stamps zero values via the
// configured Clock, after the redactor runs); a zero timestamp here is
// treated as a contract violation and surfaced rather than silently
// rendering as the Go epoch — the only path to zero is a redactor that
// reconstructs the event and forgets to carry the timestamp forward.
func formatEvent(e capture.Event) (string, error) {
	ts := e.Timestamp
	if ts.IsZero() {
		return "", errors.New("demarkus capture: zero event timestamp")
	}
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}
	var b strings.Builder
	b.WriteString("\n## ")
	b.WriteString(ts.UTC().Format("15:04:05"))
	b.WriteString(" ")
	b.WriteString(e.Kind)
	b.WriteString("\n\n```json\n")
	b.Write(payload)
	b.WriteString("\n```\n")
	return b.String(), nil
}

// capEventFields caps every string value in Payload at maxBytes,
// recursively descending into nested map[string]any and []any containers
// so a long string nested under a validator or stages payload cannot
// bypass the size boundary. Non-string leaves pass through untouched so
// structured counts and numbers keep full precision. maxBytes <= 0
// disables the cap.
//
// Truncation is rune-aware: if maxBytes would land inside a multi-byte
// UTF-8 sequence, the cut retreats to the preceding rune boundary so the
// stored payload is always valid UTF-8. The truncation marker itself
// contains the "…" rune (U+2026) and is appended after the safe cut.
func capEventFields(e capture.Event, maxBytes int) capture.Event {
	if maxBytes <= 0 || len(e.Payload) == 0 {
		return e
	}
	capped := make(map[string]any, len(e.Payload))
	for k, v := range e.Payload {
		capped[k] = capValue(v, maxBytes)
	}
	e.Payload = capped
	return e
}

// capValue returns v with every string leaf truncated to maxBytes,
// recursing through string-keyed maps and slices/arrays of any element
// type. Non-container, non-string leaves (numbers, bools, nil) pass
// through unchanged — JSON marshalling handles them as-is and they
// carry no unbounded-growth risk.
//
// The fast paths (string, map[string]any, []any, []map[string]any) cover
// every shape the session package produces; the reflect fallback
// catches typed containers a future caller might build (e.g. []string,
// map[string]string, [][]any) so the size cap remains a true boundary,
// not a convention.
func capValue(v any, maxBytes int) any {
	switch x := v.(type) {
	case string:
		if len(x) > maxBytes {
			return truncateAtRune(x, maxBytes) + "…[truncated]"
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, child := range x {
			out[k] = capValue(child, maxBytes)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, child := range x {
			out[i] = capValue(child, maxBytes)
		}
		return out
	case []map[string]any:
		// Frequent in our emit paths (e.g. validator stages), so
		// handle it explicitly — a type switch on []any would miss it.
		out := make([]map[string]any, len(x))
		for i, child := range x {
			capped := make(map[string]any, len(child))
			for k, v := range child {
				capped[k] = capValue(v, maxBytes)
			}
			out[i] = capped
		}
		return out
	case nil, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		// Scalar fast path — skip the reflect fallback for primitives
		// since they're the bulk of non-string leaves in real payloads.
		return v
	default:
		return capReflect(v, maxBytes)
	}
}

// capReflect handles typed containers (e.g. []string, map[string]string)
// that the type switch can't match cheaply. Containers are rewritten as
// []any / map[string]any, which serialises identically via json.Marshal
// while letting the recursion reach string leaves. Non-container types
// (structs, funcs, channels, etc.) pass through unchanged — they aren't
// JSON-meaningful payload values anyway.
func capReflect(v any, maxBytes int) any {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return v
	}
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := range rv.Len() {
			out[i] = capValue(rv.Index(i).Interface(), maxBytes)
		}
		return out
	case reflect.Map:
		// Only walk maps with string keys — JSON encodable maps are
		// always string-keyed, so non-string-keyed maps aren't valid
		// Payload values and we leave them untouched.
		if rv.Type().Key().Kind() != reflect.String {
			return v
		}
		out := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			out[iter.Key().String()] = capValue(iter.Value().Interface(), maxBytes)
		}
		return out
	default:
		return v
	}
}

// truncateAtRune returns s[:n'] where n' <= n and s[:n'] is valid UTF-8.
// If n lands on a continuation byte, the function walks backwards up to
// three bytes (the longest UTF-8 continuation tail) until it reaches a
// rune-start byte. Callers guarantee n <= len(s).
func truncateAtRune(s string, n int) string {
	for n > 0 && n < len(s) && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
