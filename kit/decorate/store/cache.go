// Package store hosts bundled [github.com/latebit-io/nib/kit.StoreDecorator]
// implementations. Import as `storedec "github.com/latebit-io/nib/kit/decorate/store"`
// to wrap a [memory.Store] with caching, tracing, retry, etc.
package store

import (
	"context"
	"sync"
	"time"

	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/memory"
)

// CachePolicy configures [WithCaching]. Zero MaxEntries means
// "unlimited"; zero TTL means "no expiry (cache until invalidated)."
// At least one of TTL or MaxEntries should be set in practice — an
// uncapped cache that never expires grows without bound.
type CachePolicy struct {
	// TTL is the maximum age of a cached Fetch result. Zero means
	// entries never expire on their own (still invalidated by writes).
	TTL time.Duration

	// MaxEntries caps the number of cached Fetch results. When the
	// cache reaches this size, the next miss evicts the oldest entry
	// (insertion order, not LRU — a simpler discipline that matches
	// the workload: memory paths are accessed predictably, not
	// hot/cold).
	MaxEntries int

	// Now is the clock source. Defaults to [time.Now] when nil.
	// Test-only seam; production callers should leave it nil.
	Now func() time.Time
}

// WithCaching returns a [kit.StoreDecorator] that caches
// [memory.Store.Fetch] results in-memory. Writes (Publish/Append) on
// the same path invalidate the cached entry so the next Fetch reads
// through — the write response is never cached, because adapters (the
// demarkus MCP adapter, for one) return header-only documents with an
// empty Body from writes. [memory.Store.List] passes through uncached —
// invalidating a directory listing on every child write would either
// require tracking which entries cover which paths (complex) or
// invalidating the whole cache on any write (counterproductive).
//
// Recommended chain position: closest to the base store. Caching
// inside tracing means tracing observes cache hits; caching outside
// retry means retry only fires on cache misses.
//
// Caching only Fetch is the cautious default. Callers that want to
// also cache List results should write their own decorator — the
// semantics are subtle enough that the kit shouldn't bake a guess in.
func WithCaching(policy CachePolicy) kit.StoreDecorator {
	if policy.Now == nil {
		policy.Now = time.Now
	}
	return func(inner memory.Store) memory.Store {
		return &cachedStore{
			inner:   inner,
			policy:  policy,
			entries: make(map[string]cacheEntry),
		}
	}
}

// cacheEntry is one memoized Fetch result with insertion bookkeeping
// for eviction.
type cacheEntry struct {
	doc        memory.Document
	cachedAt   time.Time
	insertedAt time.Time
}

// cachedStore implements [memory.Store] with a TTL+capacity cache in
// front of Fetch. Publish/Append delegate to the inner store and then
// invalidate the entry for the affected path, whether or not the write
// succeeded.
type cachedStore struct {
	inner  memory.Store
	policy CachePolicy

	mu      sync.Mutex
	entries map[string]cacheEntry
}

// Compile-time assertion that cachedStore satisfies the port.
var _ memory.Store = (*cachedStore)(nil)

// Fetch returns the cached document when present and unexpired;
// otherwise delegates and caches the result.
func (c *cachedStore) Fetch(ctx context.Context, path string) (memory.Document, error) {
	c.mu.Lock()
	if ent, ok := c.entries[path]; ok && !c.expired(ent, c.policy.Now()) {
		c.mu.Unlock()
		return ent.doc, nil
	}
	c.mu.Unlock()

	doc, err := c.inner.Fetch(ctx, path)
	if err != nil {
		return memory.Document{}, err
	}
	// Stamp cachedAt with a fresh Now() AFTER the inner Fetch completes,
	// so a slow inner call doesn't burn TTL it never spent serving.
	c.store(path, doc, c.policy.Now())
	return doc, nil
}

// Publish delegates and invalidates the cache entry for path. The
// returned document is not cached: write responses may be header-only.
func (c *cachedStore) Publish(ctx context.Context, path string, body string, expectedVersion int) (memory.Document, error) {
	doc, err := c.inner.Publish(ctx, path, body, expectedVersion)
	c.invalidate(path)
	if err != nil {
		return memory.Document{}, err
	}
	return doc, nil
}

// Append delegates and invalidates the cache entry for path. The
// returned document is not cached: write responses may be header-only.
func (c *cachedStore) Append(ctx context.Context, path string, body string, expectedVersion int) (memory.Document, error) {
	doc, err := c.inner.Append(ctx, path, body, expectedVersion)
	c.invalidate(path)
	if err != nil {
		return memory.Document{}, err
	}
	return doc, nil
}

// List passes through; directory listings are not cached.
func (c *cachedStore) List(ctx context.Context, path string) ([]string, error) {
	return c.inner.List(ctx, path)
}

// expired reports whether ent is past its TTL. Zero TTL means never
// expires on age alone.
func (c *cachedStore) expired(ent cacheEntry, now time.Time) bool {
	if c.policy.TTL <= 0 {
		return false
	}
	return now.Sub(ent.cachedAt) >= c.policy.TTL
}

// store inserts or refreshes the cache entry for path. Enforces
// MaxEntries by evicting the oldest entry on capacity overflow.
func (c *cachedStore) store(path string, doc memory.Document, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ent, exists := c.entries[path]
	if !exists {
		ent = cacheEntry{insertedAt: now}
		if c.policy.MaxEntries > 0 && len(c.entries) >= c.policy.MaxEntries {
			c.evictOldestLocked()
		}
	}
	ent.doc = doc
	ent.cachedAt = now
	c.entries[path] = ent
}

// invalidate drops the cache entry for path. Safe to call when the
// entry doesn't exist.
func (c *cachedStore) invalidate(path string) {
	c.mu.Lock()
	delete(c.entries, path)
	c.mu.Unlock()
}

// evictOldestLocked removes the entry with the oldest insertedAt. Caller
// must hold mu. No-op if the cache is empty.
func (c *cachedStore) evictOldestLocked() {
	var oldestKey string
	var oldestTime time.Time
	first := true
	for k, v := range c.entries {
		if first || v.insertedAt.Before(oldestTime) {
			oldestKey = k
			oldestTime = v.insertedAt
			first = false
		}
	}
	if !first {
		delete(c.entries, oldestKey)
	}
}

// Stats returns observability about the live cache. Exposed for tests
// and for kit consumers that want to expose cache hit metrics; the
// numbers are point-in-time and may shift under concurrent calls.
func (c *cachedStore) Stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{Size: len(c.entries)}
}

// CacheStats is the read-only snapshot returned by [cachedStore.Stats].
type CacheStats struct {
	// Size is the current number of cached entries.
	Size int
}
