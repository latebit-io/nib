package agent

import (
	"path/filepath"
	"testing"

	"github.com/latebit-io/nib/coding/event"
)

// subscribeForTest registers a generous-buffer [Subscription] on ag's
// bus and returns its inbox channel. The Subscription is closed via
// t.Cleanup so it never outlives the test.
//
// Tests that used to read from the legacy `events` channel passed to
// [agent.New] migrate to this helper after the channel field was
// removed in commit 4 of the kit-event-bus arc. BufferSize is wide
// (256) so tests do not need to think about drop-vs-block policy under
// burst — the assertion under test is event content / ordering, not
// drop semantics. Tests that exercise drop semantics specifically
// construct their own Subscription with the relevant options.
func subscribeForTest(t *testing.T, ag *Agent) <-chan event.Event {
	t.Helper()
	sub, err := ag.Subscribe(SubscribeOptions{BufferSize: 256})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Close)
	return sub.Events()
}

// stubWorkspace is a no-op workspace used by tests that drive the
// Agent through tool registration paths and do not exercise actual
// file IO. Methods return zero values; CanonPath is identity.
type stubWorkspace struct{}

func (stubWorkspace) ReadFile(_ string) (string, error) { return "", nil }
func (stubWorkspace) ListFiles() ([]string, error)      { return nil, nil }
func (stubWorkspace) WriteFile(_, _ string) error       { return nil }
func (stubWorkspace) CanonPath(p string) string         { return p }
func (stubWorkspace) ProjectRoot() string               { return "" }

// testWorkspace is a richer workspace stub backed by an in-memory
// files map. Used by gate_test.go and validation_pipeline_test.go to
// exercise paths that read/write file content during a test scenario.
type testWorkspace struct {
	root  string
	files map[string]string
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

func (w *testWorkspace) ProjectRoot() string { return w.root }
