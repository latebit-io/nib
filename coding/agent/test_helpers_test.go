package agent

import "path/filepath"

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
