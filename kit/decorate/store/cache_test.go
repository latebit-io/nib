package store

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/contracttest"
	"github.com/latebit-io/nib/kit/memory"
)

// mockStore is an in-memory [memory.Store] with per-method call counters.
type mockStore struct {
	docs map[string]memory.Document

	fetchCalls   atomic.Int32
	publishCalls atomic.Int32
	appendCalls  atomic.Int32
	listCalls    atomic.Int32

	fetchErr   error
	publishErr error
	appendErr  error
}

func newMockStore() *mockStore {
	return &mockStore{docs: make(map[string]memory.Document)}
}

func (m *mockStore) Fetch(ctx context.Context, path string) (memory.Document, error) {
	m.fetchCalls.Add(1)
	if err := ctx.Err(); err != nil {
		return memory.Document{}, err
	}
	if m.fetchErr != nil {
		return memory.Document{}, m.fetchErr
	}
	doc, ok := m.docs[path]
	if !ok {
		return memory.Document{}, memory.ErrNotFound
	}
	return doc, nil
}

func (m *mockStore) Publish(ctx context.Context, path string, body string, expectedVersion int) (memory.Document, error) {
	m.publishCalls.Add(1)
	if err := ctx.Err(); err != nil {
		return memory.Document{}, err
	}
	if m.publishErr != nil {
		return memory.Document{}, m.publishErr
	}
	cur, exists := m.docs[path]
	if expectedVersion == 0 {
		if exists {
			return memory.Document{}, memory.ErrConflict
		}
	} else {
		if !exists {
			return memory.Document{}, memory.ErrNotFound
		}
		if expectedVersion != cur.Version {
			return memory.Document{}, memory.ErrConflict
		}
	}
	doc := memory.Document{Path: path, Body: body, Version: cur.Version + 1, Modified: "now"}
	m.docs[path] = doc
	// Write responses are header-only (empty Body), as the demarkus MCP
	// adapter returns them — a cache that stored this would serve an
	// empty document on the next Fetch.
	return memory.Document{Path: path, Version: doc.Version, Modified: doc.Modified}, nil
}

func (m *mockStore) Append(ctx context.Context, path string, body string, expectedVersion int) (memory.Document, error) {
	m.appendCalls.Add(1)
	if err := ctx.Err(); err != nil {
		return memory.Document{}, err
	}
	if m.appendErr != nil {
		return memory.Document{}, m.appendErr
	}
	cur, exists := m.docs[path]
	if !exists {
		return memory.Document{}, memory.ErrNotFound
	}
	if expectedVersion != cur.Version {
		return memory.Document{}, memory.ErrConflict
	}
	cur.Body += body
	cur.Version = expectedVersion + 1
	cur.Path = path
	m.docs[path] = cur
	return memory.Document{Path: path, Version: cur.Version, Modified: cur.Modified}, nil
}

func (m *mockStore) List(ctx context.Context, dir string) ([]string, error) {
	m.listCalls.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0)
	for p := range m.docs {
		if strings.HasPrefix(p, dir) {
			out = append(out, p)
		}
	}
	return out, nil
}

func TestWithCaching_HitsCacheOnRepeatFetch(t *testing.T) {
	inner := newMockStore()
	inner.docs["/foo.md"] = memory.Document{Path: "/foo.md", Body: "hello", Version: 1}

	s := kit.DecorateStore(inner, WithCaching(CachePolicy{TTL: time.Hour}))

	for i := 0; i < 5; i++ {
		doc, err := s.Fetch(context.Background(), "/foo.md")
		if err != nil {
			t.Fatalf("Fetch failed on call %d: %v", i, err)
		}
		if doc.Body != "hello" {
			t.Fatalf("call %d: body = %q, want %q", i, doc.Body, "hello")
		}
	}
	if got := inner.fetchCalls.Load(); got != 1 {
		t.Fatalf("inner.Fetch called %d times, expected 1 (rest from cache)", got)
	}
}

func TestWithCaching_TTLExpiry(t *testing.T) {
	now := time.Now()
	clock := &now
	inner := newMockStore()
	inner.docs["/foo.md"] = memory.Document{Path: "/foo.md", Body: "v1", Version: 1}

	s := kit.DecorateStore(inner, WithCaching(CachePolicy{
		TTL: 100 * time.Millisecond,
		Now: func() time.Time { return *clock },
	}))

	if _, err := s.Fetch(context.Background(), "/foo.md"); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	// Inside TTL: still cached.
	*clock = now.Add(50 * time.Millisecond)
	if _, err := s.Fetch(context.Background(), "/foo.md"); err != nil {
		t.Fatalf("cached Fetch: %v", err)
	}
	if got := inner.fetchCalls.Load(); got != 1 {
		t.Fatalf("expected 1 inner call within TTL, got %d", got)
	}
	// Past TTL: refetches.
	*clock = now.Add(200 * time.Millisecond)
	if _, err := s.Fetch(context.Background(), "/foo.md"); err != nil {
		t.Fatalf("post-TTL Fetch: %v", err)
	}
	if got := inner.fetchCalls.Load(); got != 2 {
		t.Fatalf("expected refetch after TTL, got %d inner calls", got)
	}
}

func TestWithCaching_TTLStartsFromFetchCompletion(t *testing.T) {
	// Simulate a slow inner Fetch by advancing the clock inside the
	// mock. If cachedAt were stamped at the start of the wrapper's
	// Fetch (the pre-fix behavior), the entry's TTL would be measured
	// from before the slow call — a 10s TTL behind a 3s inner call
	// would survive only ~7s. After the fix, TTL is measured from
	// completion, so the entry survives the full 10s after the inner
	// call returns.
	now := time.Now()
	clock := &now
	innerFetchDelay := 3 * time.Second
	inner := newMockStore()
	inner.docs["/slow.md"] = memory.Document{Path: "/slow.md", Body: "v1", Version: 1}
	// Wrap Fetch so it advances the clock to model latency.
	delayingInner := &delayingFetchStore{
		inner: inner,
		onFetch: func() {
			*clock = clock.Add(innerFetchDelay)
		},
	}
	s := kit.DecorateStore(delayingInner, WithCaching(CachePolicy{
		TTL: 10 * time.Second,
		Now: func() time.Time { return *clock },
	}))

	if _, err := s.Fetch(context.Background(), "/slow.md"); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	// Advance to 9s after fetch completion — still inside the 10s TTL
	// when measured from completion; would already be past it if TTL
	// were measured from the start of the wrapper call (3 + 9 = 12s).
	*clock = clock.Add(9 * time.Second)
	if _, err := s.Fetch(context.Background(), "/slow.md"); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if got := inner.fetchCalls.Load(); got != 1 {
		t.Fatalf("expected TTL measured from fetch completion (1 inner call), got %d", got)
	}
}

func TestWithCaching_InFlightFetchDoesNotCacheStaleDoc(t *testing.T) {
	// A Fetch reads v1, then a Publish (v2) lands and invalidates before
	// the Fetch stores its result. The stale v1 must not be cached: the
	// next Fetch has to read through and observe v2.
	inner := newMockStore()
	inner.docs["/doc.md"] = memory.Document{Path: "/doc.md", Body: "v1", Version: 1}
	racing := &delayingFetchStore{inner: inner}
	s := kit.DecorateStore(racing, WithCaching(CachePolicy{}))
	var once bool
	racing.onFetch = func() {
		if once {
			return
		}
		once = true
		if _, err := s.Publish(context.Background(), "/doc.md", "v2", 1); err != nil {
			t.Errorf("racing Publish: %v", err)
		}
	}

	first, err := s.Fetch(context.Background(), "/doc.md")
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	if first.Body != "v1" {
		t.Fatalf("first Fetch read %q before the write; want v1", first.Body)
	}
	second, err := s.Fetch(context.Background(), "/doc.md")
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if second.Body != "v2" {
		t.Fatalf("second Fetch = %q; stale v1 was cached past the write", second.Body)
	}
	if got := inner.fetchCalls.Load(); got != 2 {
		t.Fatalf("inner fetch calls = %d, want 2 (stale fill discarded)", got)
	}
}

func TestWithCaching_PublishInvalidates(t *testing.T) {
	inner := newMockStore()
	inner.docs["/foo.md"] = memory.Document{Path: "/foo.md", Body: "v1", Version: 1}
	s := kit.DecorateStore(inner, WithCaching(CachePolicy{TTL: time.Hour}))

	// Prime the cache.
	if _, err := s.Fetch(context.Background(), "/foo.md"); err != nil {
		t.Fatalf("prime: %v", err)
	}
	// Publish a new version through the decorator.
	if _, err := s.Publish(context.Background(), "/foo.md", "v2", 1); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Fetch should see the new body — either from updated cache or
	// from a refetch; in either case the body must be v2.
	doc, err := s.Fetch(context.Background(), "/foo.md")
	if err != nil {
		t.Fatalf("post-publish fetch: %v", err)
	}
	if doc.Body != "v2" {
		t.Fatalf("got body %q, want v2 — stale cache leaked", doc.Body)
	}
}

func TestWithCaching_AppendInvalidates(t *testing.T) {
	inner := newMockStore()
	inner.docs["/foo.md"] = memory.Document{Path: "/foo.md", Body: "v1", Version: 1}
	s := kit.DecorateStore(inner, WithCaching(CachePolicy{TTL: time.Hour}))

	if _, err := s.Fetch(context.Background(), "/foo.md"); err != nil {
		t.Fatalf("prime: %v", err)
	}
	if _, err := s.Append(context.Background(), "/foo.md", " more", 1); err != nil {
		t.Fatalf("append: %v", err)
	}
	doc, err := s.Fetch(context.Background(), "/foo.md")
	if err != nil {
		t.Fatalf("post-append fetch: %v", err)
	}
	if doc.Body != "v1 more" {
		t.Fatalf("got body %q, want v1 more — cache did not refresh", doc.Body)
	}
}

func TestWithCaching_PublishErrorInvalidatesEntry(t *testing.T) {
	inner := newMockStore()
	inner.docs["/foo.md"] = memory.Document{Path: "/foo.md", Body: "v1", Version: 1}
	s := kit.DecorateStore(inner, WithCaching(CachePolicy{TTL: time.Hour}))

	if _, err := s.Fetch(context.Background(), "/foo.md"); err != nil {
		t.Fatalf("prime: %v", err)
	}
	inner.publishErr = errors.New("upstream failed")
	if _, err := s.Publish(context.Background(), "/foo.md", "v2", 1); err == nil {
		t.Fatal("expected publish error")
	}
	// Inner doc still v1 (publish failed) but cache should have been
	// dropped, so next Fetch hits the inner store.
	before := inner.fetchCalls.Load()
	if _, err := s.Fetch(context.Background(), "/foo.md"); err != nil {
		t.Fatalf("post-failed-publish fetch: %v", err)
	}
	if got := inner.fetchCalls.Load(); got <= before {
		t.Fatalf("expected refetch after failed publish; inner fetch count did not advance (%d -> %d)", before, got)
	}
}

func TestWithCaching_MaxEntriesEviction(t *testing.T) {
	now := time.Now()
	tick := func() time.Time {
		now = now.Add(time.Millisecond)
		return now
	}
	inner := newMockStore()
	for i := 0; i < 5; i++ {
		path := pathN(i)
		inner.docs[path] = memory.Document{Path: path, Body: path, Version: 1}
	}
	s := kit.DecorateStore(inner, WithCaching(CachePolicy{
		MaxEntries: 2,
		TTL:        time.Hour,
		Now:        tick,
	}))

	// Prime cache with three paths; only the latter two should remain.
	for i := 0; i < 3; i++ {
		if _, err := s.Fetch(context.Background(), pathN(i)); err != nil {
			t.Fatalf("Fetch %d: %v", i, err)
		}
	}
	// /foo0.md should have been evicted; refetching it forces an inner call.
	before := inner.fetchCalls.Load()
	if _, err := s.Fetch(context.Background(), pathN(0)); err != nil {
		t.Fatalf("re-fetch evicted: %v", err)
	}
	if got := inner.fetchCalls.Load(); got != before+1 {
		t.Fatalf("expected eviction-driven refetch; inner fetches did not advance")
	}
}

// TestCachedStore_SatisfiesContract verifies the caching decorator
// preserves the [memory.Store] contract when wrapping a conforming
// inner. The mockStore inner is fully version-aware so the contract
// suite exercises read-through caching plus the invariants the
// decorator must preserve (not-found sentinel, conflict semantics,
// list visibility, ctx cancellation).
func TestCachedStore_SatisfiesContract(t *testing.T) {
	contracttest.Store(t, func() memory.Store {
		return kit.DecorateStore(newMockStore(), WithCaching(CachePolicy{TTL: time.Hour}))
	})
}

func TestWithCaching_ListNotCached(t *testing.T) {
	inner := newMockStore()
	s := kit.DecorateStore(inner, WithCaching(CachePolicy{TTL: time.Hour}))
	for i := 0; i < 3; i++ {
		if _, err := s.List(context.Background(), "/"); err != nil {
			t.Fatalf("List: %v", err)
		}
	}
	if got := inner.listCalls.Load(); got != 3 {
		t.Fatalf("List should be uncached; got %d inner calls, want 3", got)
	}
}

func pathN(i int) string {
	return "/foo" + string(rune('0'+i)) + ".md"
}

// delayingFetchStore wraps a mockStore and runs onFetch after the inner
// read but before Fetch returns. Used to simulate slow upstream stores
// (TTL tests) and writes that race an in-flight read (staleness tests).
type delayingFetchStore struct {
	inner   *mockStore
	onFetch func()
}

func (d *delayingFetchStore) Fetch(ctx context.Context, path string) (memory.Document, error) {
	doc, err := d.inner.Fetch(ctx, path)
	if d.onFetch != nil {
		d.onFetch()
	}
	return doc, err
}

func (d *delayingFetchStore) Publish(ctx context.Context, path string, body string, expectedVersion int) (memory.Document, error) {
	return d.inner.Publish(ctx, path, body, expectedVersion)
}

func (d *delayingFetchStore) Append(ctx context.Context, path string, body string, expectedVersion int) (memory.Document, error) {
	return d.inner.Append(ctx, path, body, expectedVersion)
}

func (d *delayingFetchStore) List(ctx context.Context, path string) ([]string, error) {
	return d.inner.List(ctx, path)
}
