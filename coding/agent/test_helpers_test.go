package agent

import (
	"path/filepath"
	"testing"

	"github.com/latebit-io/nib/engine/event"
)

// mustDrainEvents returns a buffered events channel with a background
// drainer that auto-resolves [event.FlushBuffers] requests. Tests
// that exercise [Agent.FoundationHooks].BeforeToolCall directly use
// this so the flush-dirty-buffers step inside the hook does not stall
// for its 5-second response timeout.
//
// The drainer also keeps the channel from filling up — control-flow
// events ([event.AgentEditProposed], [event.AgentDone]) would
// otherwise block past their 5-second deliver timeout and cascade
// failures into apparently-unrelated tests.
func mustDrainEvents(t *testing.T) chan event.Event {
	t.Helper()
	ch := make(chan event.Event, 128)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					return
				}
				if fb, ok := ev.(event.FlushBuffers); ok {
					select {
					case fb.Result <- event.FlushResult{}:
					default:
					}
				}
			case <-done:
				return
			}
		}
	}()
	t.Cleanup(func() { close(done) })
	return ch
}

// stubWorkspace is a no-op workspace used by tests that drive the
// Agent through tool registration paths and do not exercise actual
// file IO. Methods return zero values; CanonPath is identity.
type stubWorkspace struct{}

func (stubWorkspace) ReadFile(_ string) (string, error) { return "", nil }
func (stubWorkspace) ListFiles() ([]string, error)      { return nil, nil }
func (stubWorkspace) WriteFile(_, _ string) error       { return nil }
func (stubWorkspace) CanonPath(p string) string         { return p }
func (stubWorkspace) InContext(_ string) bool           { return true }
func (stubWorkspace) AddContext(_ string)               {}
func (stubWorkspace) ProjectRoot() string               { return "" }

// testWorkspace is a richer workspace stub backed by an in-memory
// files map. Used by gate_test.go and validation_pipeline_test.go to
// exercise paths that read/write file content during a test scenario.
type testWorkspace struct {
	root            string
	files           map[string]string
	inContext       map[string]bool
	addContextCalls int
}

func (w *testWorkspace) ReadFile(path string) (string, error) {
	if c, ok := w.files[path]; ok {
		return c, nil
	}
	return "", nil
}

func (w *testWorkspace) ListFiles() ([]string, error) { return nil, nil }
func (w *testWorkspace) WriteFile(_, _ string) error  { return nil }
func (w *testWorkspace) CanonPath(p string) string {
	if w.root != "" && !filepath.IsAbs(p) {
		return filepath.Clean(filepath.Join(w.root, p))
	}
	return p
}

func (w *testWorkspace) InContext(path string) bool {
	if w.inContext == nil {
		return false
	}
	return w.inContext[w.CanonPath(path)]
}

func (w *testWorkspace) AddContext(path string) {
	w.addContextCalls++
	if w.inContext == nil {
		w.inContext = make(map[string]bool)
	}
	w.inContext[w.CanonPath(path)] = true
}

func (w *testWorkspace) ProjectRoot() string { return w.root }

// stubTracker satisfies TaskTracker with no-op writes and zero-value
// reads. Tests that need scripted behavior (e.g. NextPendingTask
// returning a specific title) embed *stubTracker and override the
// relevant method.
type stubTracker struct{}

func (*stubTracker) ActivateTask(string) error          { return nil }
func (*stubTracker) CompleteTask(string) error          { return nil }
func (*stubTracker) AddTask(_, _, _, _ string) error    { return nil }
func (*stubTracker) ActiveTaskPath() string             { return "" }
func (*stubTracker) WorkTreeLoaded() bool               { return true }
func (*stubTracker) NextPendingTask() string            { return "" }
func (*stubTracker) InitProject(string, []string) error { return nil }
