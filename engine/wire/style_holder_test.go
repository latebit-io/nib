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
