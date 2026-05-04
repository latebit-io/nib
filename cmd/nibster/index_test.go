package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/latebit-io/nib/kit/memory"
)

func TestFormatEntry(t *testing.T) {
	t.Parallel()

	got := formatEntry("2026-05-04-143045-build-an-e-mower", "How can I build an e-mower?", statusSuccess)
	want := `- [2026-05-04-143045-build-an-e-mower](sessions/2026-05-04-143045-build-an-e-mower.md) — "How can I build an e-mower?" — ok` + "\n"
	if got != want {
		t.Errorf("formatEntry = %q\nwant %q", got, want)
	}
}

func TestQuoteMessage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello", `"hello"`},
		{"newline-collapses", "line1\nline2", `"line1 line2"`},
		{"crlf-collapses", "line1\r\nline2", `"line1 line2"`},
		{"bare-cr-collapses", "line1\rline2", `"line1 line2"`},
		{"truncates-long", strings.Repeat("a", 200), `"` + strings.Repeat("a", messageMaxLen-3) + `..."`},
		{"escapes-quote", `say "hi"`, `"say \"hi\""`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := quoteMessage(tc.in)
			if got != tc.want {
				t.Errorf("quoteMessage(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// fakeStore is an in-memory implementation of memory.Store for tests.
// Sufficient for index-write and read-back coverage without spinning up
// demarkus.
type fakeStore struct {
	mu   sync.Mutex
	docs map[string]memory.Document
}

func newFakeStore() *fakeStore {
	return &fakeStore{docs: make(map[string]memory.Document)}
}

func (s *fakeStore) Fetch(_ context.Context, path string) (memory.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, ok := s.docs[path]
	if !ok {
		return memory.Document{}, memory.ErrNotFound
	}
	return doc, nil
}

func (s *fakeStore) Publish(_ context.Context, path, body string, expectedVersion int) (memory.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, exists := s.docs[path]
	switch {
	case expectedVersion == 0 && exists:
		return memory.Document{}, memory.ErrConflict
	case expectedVersion > 0 && !exists:
		return memory.Document{}, memory.ErrNotFound
	case expectedVersion > 0 && cur.Version != expectedVersion:
		return memory.Document{}, memory.ErrConflict
	}
	doc := memory.Document{Path: path, Body: body, Version: cur.Version + 1}
	s.docs[path] = doc
	return doc, nil
}

func (s *fakeStore) Append(_ context.Context, path, body string, expectedVersion int) (memory.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.docs[path]
	if !ok {
		return memory.Document{}, memory.ErrNotFound
	}
	if cur.Version != expectedVersion {
		return memory.Document{}, memory.ErrConflict
	}
	doc := memory.Document{Path: path, Body: cur.Body + body, Version: cur.Version + 1}
	s.docs[path] = doc
	return doc, nil
}

func (s *fakeStore) List(_ context.Context, prefix string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.docs))
	for p := range s.docs {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	return out, nil
}

func TestWriteIndexEntry_CreatesFreshIndex(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	ctx := t.Context()

	if err := writeIndexEntry(ctx, store, "2026-05-04-143045-first", "first run", statusSuccess); err != nil {
		t.Fatalf("writeIndexEntry: %v", err)
	}

	doc, err := store.Fetch(ctx, indexPath)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.HasPrefix(doc.Body, indexHeader) {
		t.Errorf("body missing header: %q", doc.Body)
	}
	if !strings.Contains(doc.Body, "2026-05-04-143045-first") {
		t.Errorf("body missing session id: %q", doc.Body)
	}
	if !strings.Contains(doc.Body, "ok") {
		t.Errorf("body missing status: %q", doc.Body)
	}
}

func TestWriteIndexEntry_AppendsToExistingIndex(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	ctx := t.Context()

	if err := writeIndexEntry(ctx, store, "first", "first run", statusSuccess); err != nil {
		t.Fatalf("write first: %v", err)
	}
	if err := writeIndexEntry(ctx, store, "second", "second run", statusFailed); err != nil {
		t.Fatalf("write second: %v", err)
	}

	doc, err := store.Fetch(ctx, indexPath)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(doc.Body, "first") {
		t.Errorf("first entry missing: %q", doc.Body)
	}
	if !strings.Contains(doc.Body, "second") {
		t.Errorf("second entry missing: %q", doc.Body)
	}
	if !strings.Contains(doc.Body, "fail") {
		t.Errorf("second status missing: %q", doc.Body)
	}
	if doc.Version != 2 {
		t.Errorf("version = %d, want 2 (one publish + one append)", doc.Version)
	}
}

func TestWriteIndexEntry_SurfacesFetchError(t *testing.T) {
	t.Parallel()

	want := errors.New("boom")
	store := errStore{err: want}
	err := writeIndexEntry(t.Context(), store, "id", "msg", statusSuccess)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want chain to contain %v", err, want)
	}
}

// errStore returns a fixed error from Fetch so we can test the error path.
type errStore struct{ err error }

func (s errStore) Fetch(_ context.Context, _ string) (memory.Document, error) {
	return memory.Document{}, s.err
}
func (s errStore) Publish(_ context.Context, _, _ string, _ int) (memory.Document, error) {
	return memory.Document{}, s.err
}
func (s errStore) Append(_ context.Context, _, _ string, _ int) (memory.Document, error) {
	return memory.Document{}, s.err
}
func (s errStore) List(_ context.Context, _ string) ([]string, error) {
	return nil, s.err
}

// flakyStore wraps fakeStore and returns ErrConflict on the first N
// Append calls before delegating normally. Used to verify that
// [writeIndexEntry] retries the optimistic-concurrency path.
type flakyStore struct {
	*fakeStore
	conflictsRemaining int
	mu                 sync.Mutex
}

func (s *flakyStore) Append(ctx context.Context, path, body string, expectedVersion int) (memory.Document, error) {
	s.mu.Lock()
	if s.conflictsRemaining > 0 {
		s.conflictsRemaining--
		s.mu.Unlock()
		return memory.Document{}, memory.ErrConflict
	}
	s.mu.Unlock()
	return s.fakeStore.Append(ctx, path, body, expectedVersion)
}

func TestWriteIndexEntry_RetriesOnConflict(t *testing.T) {
	t.Parallel()

	base := newFakeStore()
	// Pre-create the index so writeIndexEntry takes the Append path.
	if _, err := base.Publish(t.Context(), indexPath, indexHeader, 0); err != nil {
		t.Fatalf("seed index: %v", err)
	}

	flaky := &flakyStore{fakeStore: base, conflictsRemaining: 2}
	if err := writeIndexEntry(t.Context(), flaky, "id", "msg", statusSuccess); err != nil {
		t.Fatalf("writeIndexEntry: %v", err)
	}
	doc, err := base.Fetch(t.Context(), indexPath)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(doc.Body, "id") {
		t.Errorf("entry not appended after retries: %q", doc.Body)
	}
}

func TestWriteIndexEntry_GivesUpAfterRepeatedConflicts(t *testing.T) {
	t.Parallel()

	base := newFakeStore()
	if _, err := base.Publish(t.Context(), indexPath, indexHeader, 0); err != nil {
		t.Fatalf("seed index: %v", err)
	}

	// More conflicts than the retry budget — writeIndexEntry should
	// give up and return a wrapped ErrConflict.
	flaky := &flakyStore{fakeStore: base, conflictsRemaining: indexWriteRetries + 1}
	err := writeIndexEntry(t.Context(), flaky, "id", "msg", statusSuccess)
	if err == nil {
		t.Fatalf("expected error after exhausted retries")
	}
	if !errors.Is(err, memory.ErrConflict) {
		t.Errorf("err = %v, want chain to contain memory.ErrConflict", err)
	}
}
