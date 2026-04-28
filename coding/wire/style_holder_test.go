package wire

import (
	"context"
	"sync"
	"testing"

	"github.com/latebit-io/junto/engine/lint"
)

// stubLinter is the minimal Linter implementation tests need.
type stubLinter struct{ name string }

func (s *stubLinter) Name() string { return s.name }
func (s *stubLinter) Run(_ context.Context, _, _ string, _ []string) lint.Result {
	return lint.Result{}
}

// TestPerFileLinterHolderRoundTrip verifies the holder returns what
// it was set to, and that successive Set calls overwrite cleanly.
func TestPerFileLinterHolderRoundTrip(t *testing.T) {
	t.Parallel()

	h := NewPerFileLinterHolder([]lint.Linter{&stubLinter{name: "first"}})
	if got := h.Linters(); len(got) != 1 || got[0].Name() != "first" {
		t.Fatalf("Linters() = %+v, want [first]", got)
	}

	h.Set([]lint.Linter{&stubLinter{name: "second"}, &stubLinter{name: "third"}})
	got := h.Linters()
	if len(got) != 2 || got[0].Name() != "second" || got[1].Name() != "third" {
		t.Errorf("Linters() = %+v, want [second, third]", got)
	}
}

// TestPerFileLinterHolderSetNilDisables verifies Set(nil) clears the
// holder cleanly — used when the developer cycles past the last
// style with Alt+S.
func TestPerFileLinterHolderSetNilDisables(t *testing.T) {
	t.Parallel()

	h := NewPerFileLinterHolder([]lint.Linter{&stubLinter{name: "x"}})
	h.Set(nil)
	if got := h.Linters(); got != nil {
		t.Errorf("Linters() = %+v, want nil after Set(nil)", got)
	}
}

// TestPerFileLinterHolderRaceSafe verifies concurrent Set + Linters
// is safe under -race. Without the mutex this would trip the race
// detector immediately.
func TestPerFileLinterHolderRaceSafe(t *testing.T) {
	t.Parallel()

	h := NewPerFileLinterHolder([]lint.Linter{&stubLinter{name: "initial"}})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 100 {
			h.Set([]lint.Linter{&stubLinter{name: "a"}})
			h.Set([]lint.Linter{&stubLinter{name: "b"}})
		}
	}()
	go func() {
		defer wg.Done()
		for range 100 {
			_ = h.Linters()
		}
	}()
	wg.Wait()
}

// TestPerFileLinterHolderDefensiveCopyOnSet verifies that a caller
// who later mutates the slice they passed into Set cannot affect
// the holder's stored value. Without the defensive copy, a future
// caller pattern like
//
//	tmp := buildLinters()
//	holder.Set(tmp)
//	tmp[0] = nil  // accidental teardown
//
// would race with concurrent Linters() readers and could put a nil
// into the iterating goroutine's path.
func TestPerFileLinterHolderDefensiveCopyOnSet(t *testing.T) {
	t.Parallel()

	caller := []lint.Linter{&stubLinter{name: "original"}}
	h := NewPerFileLinterHolder(caller)

	// Mutate the caller's slice after handing it over.
	caller[0] = &stubLinter{name: "tampered"}

	got := h.Linters()
	if len(got) != 1 {
		t.Fatalf("Linters() = %d items, want 1", len(got))
	}
	if got[0].Name() != "original" {
		t.Errorf("holder slice mutated by caller: got name %q, want %q",
			got[0].Name(), "original")
	}
}

// TestPerFileLinterHolderDefensiveCopyOnLinters verifies that a
// reader who mutates the returned slice cannot affect the holder's
// stored value. Without the defensive copy, the natural-looking
//
//	xs := holder.Linters()
//	xs = append(xs, extra)
//
// can — depending on capacity — write into the holder's backing
// array.
func TestPerFileLinterHolderDefensiveCopyOnLinters(t *testing.T) {
	t.Parallel()

	h := NewPerFileLinterHolder([]lint.Linter{&stubLinter{name: "kept"}})

	xs := h.Linters()
	xs[0] = &stubLinter{name: "tampered"}

	again := h.Linters()
	if again[0].Name() != "kept" {
		t.Errorf("holder slice mutated by reader: got name %q, want %q",
			again[0].Name(), "kept")
	}
}

// TestPerFileLinterHolderEmptyInputPreservesNil locks the
// "no per-file linter" sentinel — callers' Applicable check uses
// len() > 0, so an empty slice and nil must be observationally
// indistinguishable through the holder.
func TestPerFileLinterHolderEmptyInputPreservesNil(t *testing.T) {
	t.Parallel()

	h := NewPerFileLinterHolder([]lint.Linter{})
	if got := h.Linters(); got != nil {
		t.Errorf("Linters() = %+v on empty initial input, want nil", got)
	}

	h2 := NewPerFileLinterHolder(nil)
	if got := h2.Linters(); got != nil {
		t.Errorf("Linters() = %+v on nil initial input, want nil", got)
	}
}
