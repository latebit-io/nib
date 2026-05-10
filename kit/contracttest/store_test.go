package contracttest

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/latebit-io/nib/kit/memory"
)

// memStore is a minimal in-process [memory.Store] that fully honours
// the documented contract. Used as the canonical conforming
// implementation to verify the [Store] fixture is wired correctly.
//
// Not exported: contracttest's role is verifying implementations,
// not providing them. Implementations that want an in-memory fake
// for their own tests should write one — the contract suite verifies
// it.
type memStore struct {
	mu   sync.Mutex
	docs map[string]memory.Document
}

func newMemStore() *memStore {
	return &memStore{docs: make(map[string]memory.Document)}
}

func (m *memStore) Fetch(ctx context.Context, path string) (memory.Document, error) {
	if err := ctx.Err(); err != nil {
		return memory.Document{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	doc, ok := m.docs[path]
	if !ok {
		return memory.Document{}, memory.ErrNotFound
	}
	return doc, nil
}

func (m *memStore) Publish(ctx context.Context, path string, body string, expectedVersion int) (memory.Document, error) {
	if err := ctx.Err(); err != nil {
		return memory.Document{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
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
	next := memory.Document{Path: path, Body: body, Version: cur.Version + 1, Modified: "now"}
	m.docs[path] = next
	return next, nil
}

func (m *memStore) Append(ctx context.Context, path string, body string, expectedVersion int) (memory.Document, error) {
	if err := ctx.Err(); err != nil {
		return memory.Document{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, exists := m.docs[path]
	if !exists {
		return memory.Document{}, memory.ErrNotFound
	}
	if expectedVersion != cur.Version {
		return memory.Document{}, memory.ErrConflict
	}
	next := memory.Document{Path: path, Body: cur.Body + body, Version: cur.Version + 1, Modified: "now"}
	m.docs[path] = next
	return next, nil
}

func (m *memStore) List(ctx context.Context, dir string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0)
	for p := range m.docs {
		if strings.HasPrefix(p, dir) {
			out = append(out, p)
		}
	}
	return out, nil
}

// TestStore_HappyImplementationPasses confirms the fixture passes a
// canonical conforming store. Negative-path verification follows the
// same manual-discipline approach documented on
// TestProvider_HappyImplementationPasses.
func TestStore_HappyImplementationPasses(t *testing.T) {
	store := newMemStore()
	Store(t, func() memory.Store { return store })
}
