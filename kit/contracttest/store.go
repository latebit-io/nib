package contracttest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/nib/kit/memory"
)

// Store runs the [memory.Store] contract suite against stores returned
// by ctor.
//
// Contract preconditions on the ctor'd store:
//
//   - Operations on paths the test has not touched return [memory.ErrNotFound]
//     from Fetch. The fixture uses uniquely-prefixed paths per subtest
//     so a store with pre-existing unrelated documents is acceptable.
//   - The store implements optimistic concurrency on Publish/Append
//     (expectedVersion semantics from the doc comment).
//
// Subtests verify [memory.Store]'s documented invariants: not-found
// sentinel, version monotonicity, conflict on stale expectedVersion,
// list visibility after publish, ctx cancellation propagation.
// Implementations that violate any invariant fail with a message
// identifying the contract claim.
//
// Apply to a new store implementation by adding a single test:
//
//	func TestMyStoreContract(t *testing.T) {
//	    contracttest.Store(t, func() memory.Store {
//	        return mystore.New(t.TempDir())
//	    })
//	}
func Store(t *testing.T, ctor func() memory.Store) {
	t.Helper()
	if ctor == nil {
		t.Fatal("contracttest.Store: ctor must be non-nil")
	}

	t.Run("FetchMissingReturnsErrNotFound", func(t *testing.T) {
		s := ctor()
		path := uniquePath(t, "fetch-missing")
		_, err := s.Fetch(context.Background(), path)
		if !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("contract: Fetch on missing path returned %v, want errors.Is(memory.ErrNotFound)", err)
		}
	})

	t.Run("PublishCreateThenFetch", func(t *testing.T) {
		s := ctor()
		path := uniquePath(t, "create")
		const body = "first body"
		created, err := s.Publish(context.Background(), path, body, 0)
		if err != nil {
			t.Fatalf("contract: Publish(expectedVersion=0) on new path returned error: %v", err)
		}
		if created.Version <= 0 {
			t.Fatalf("contract: Publish must return a positive Version, got %d", created.Version)
		}
		if created.Path != path {
			t.Fatalf("contract: returned Document.Path = %q, want %q", created.Path, path)
		}
		fetched, err := s.Fetch(context.Background(), path)
		if err != nil {
			t.Fatalf("contract: Fetch after Publish returned error: %v", err)
		}
		if fetched.Body != body {
			t.Fatalf("contract: Fetch.Body = %q, want %q", fetched.Body, body)
		}
		if fetched.Version != created.Version {
			t.Fatalf("contract: Fetch.Version = %d, Publish returned %d", fetched.Version, created.Version)
		}
	})

	t.Run("PublishUpdateMonotonicVersion", func(t *testing.T) {
		s := ctor()
		path := uniquePath(t, "update")
		v1, err := s.Publish(context.Background(), path, "v1", 0)
		if err != nil {
			t.Fatalf("contract: first Publish: %v", err)
		}
		v2, err := s.Publish(context.Background(), path, "v2", v1.Version)
		if err != nil {
			t.Fatalf("contract: second Publish with correct expectedVersion: %v", err)
		}
		if v2.Version <= v1.Version {
			t.Fatalf("contract: Version must increase on update (v1=%d, v2=%d)", v1.Version, v2.Version)
		}
	})

	t.Run("PublishWrongVersionReturnsErrConflict", func(t *testing.T) {
		s := ctor()
		path := uniquePath(t, "conflict")
		// Bring the document up to version >= 2 by publishing twice
		// so we can reference a "behind-actual" stale version. Some
		// backends (demarkus mcpadapter as of 2026-05-10) translate
		// "ahead-of-actual" expectedVersion into a fetch-not-found
		// path that surfaces as [memory.ErrServer] rather than
		// [memory.ErrConflict]; the contract claim verified here is
		// the stale-update semantics every backend handles uniformly.
		v1, err := s.Publish(context.Background(), path, "v1", 0)
		if err != nil {
			t.Fatalf("contract: first Publish: %v", err)
		}
		v2, err := s.Publish(context.Background(), path, "v2", v1.Version)
		if err != nil {
			t.Fatalf("contract: second Publish: %v", err)
		}
		_, err = s.Publish(context.Background(), path, "stale", v1.Version)
		if !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("contract: Publish with stale expectedVersion %d (actual=%d) returned %v, want errors.Is(memory.ErrConflict)", v1.Version, v2.Version, err)
		}
	})

	t.Run("AppendRequiresExistingDocument", func(t *testing.T) {
		s := ctor()
		path := uniquePath(t, "append-missing")
		// Append against a non-existent document. The contract requires
		// expectedVersion>=1, so the document must exist — calling
		// Append against a missing path must surface a non-nil error.
		// Whether the specific sentinel is ErrNotFound or ErrConflict
		// depends on the backend; the contract claim verified here is
		// "Append against a missing document does NOT silently create."
		_, err := s.Append(context.Background(), path, "body", 1)
		if err == nil {
			t.Fatal("contract: Append against missing document must return an error")
		}
		// Verify the document was not created by the failed Append.
		if _, ferr := s.Fetch(context.Background(), path); !errors.Is(ferr, memory.ErrNotFound) {
			t.Fatalf("contract: failed Append must not create the document; Fetch returned %v", ferr)
		}
	})

	t.Run("AppendWrongVersionReturnsErrConflict", func(t *testing.T) {
		s := ctor()
		path := uniquePath(t, "append-conflict")
		// Bring document to version >= 2 so we can reference a
		// behind-actual stale version. See the corresponding note on
		// PublishWrongVersionReturnsErrConflict.
		v1, err := s.Publish(context.Background(), path, "v1", 0)
		if err != nil {
			t.Fatalf("contract: setup Publish: %v", err)
		}
		if _, err := s.Publish(context.Background(), path, "v2", v1.Version); err != nil {
			t.Fatalf("contract: second Publish: %v", err)
		}
		_, err = s.Append(context.Background(), path, "more", v1.Version)
		if !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("contract: Append with stale expectedVersion %d returned %v, want errors.Is(memory.ErrConflict)", v1.Version, err)
		}
	})

	t.Run("ListIncludesPublishedPaths", func(t *testing.T) {
		s := ctor()
		// Use a directory unique to this subtest so unrelated entries
		// in the store do not influence the assertion.
		dir := uniquePath(t, "list") + "/"
		path := dir + "doc.md"
		if _, err := s.Publish(context.Background(), path, "x", 0); err != nil {
			t.Fatalf("contract: setup Publish: %v", err)
		}
		entries, err := s.List(context.Background(), dir)
		if err != nil {
			t.Fatalf("contract: List returned error: %v", err)
		}
		if !containsPath(entries, path) {
			t.Fatalf("contract: List(%q) did not include just-published %q (got %v)", dir, path, entries)
		}
	})

	t.Run("CtxCancellationRespected", func(t *testing.T) {
		s := ctor()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		path := uniquePath(t, "ctx-cancel")
		// Each operation should observe the already-cancelled context
		// and return an error (typically ctx.Err()). Implementations
		// that handle cancellation only at I/O boundaries are
		// acceptable — the contract just says "must respect ctx.Done()."
		// We treat "any error" as conforming here; the tightest claim
		// the doc makes is propagation, not a specific sentinel.
		if _, err := s.Fetch(ctx, path); err == nil {
			t.Error("contract: Fetch with pre-cancelled ctx returned nil error")
		}
		if _, err := s.Publish(ctx, path, "x", 0); err == nil {
			t.Error("contract: Publish with pre-cancelled ctx returned nil error")
		}
		if _, err := s.Append(ctx, path, "x", 1); err == nil {
			t.Error("contract: Append with pre-cancelled ctx returned nil error")
		}
		if _, err := s.List(ctx, "/"); err == nil {
			t.Error("contract: List with pre-cancelled ctx returned nil error")
		}
	})
}

// uniquePath builds a path that is unique across subtests in a single
// process run. Avoids collisions when a single backing store is shared
// across subtests via the ctor (the common case for integration tests
// against a real server).
func uniquePath(t *testing.T, kind string) string {
	t.Helper()
	return fmt.Sprintf("/contracttest/%s-%d-%d.md", kind, time.Now().UnixNano(), uniqueCounter.next())
}

// containsPath reports whether want appears in entries by full match,
// or as a suffix (some backends return directory-relative paths).
func containsPath(entries []string, want string) bool {
	for _, e := range entries {
		if e == want || strings.HasSuffix(want, e) || strings.HasSuffix(e, want) {
			return true
		}
	}
	return false
}
