package session

import (
	"context"
	"errors"
	"testing"

	"github.com/latebit-io/junto/engine/memory"
	"github.com/latebit-io/junto/engine/project"
)

// mockMemoryStore implements the memoryStore interface for testing.
type mockMemoryStore struct {
	docs map[string]memory.Document
}

func newMockMemoryStore() *mockMemoryStore {
	return &mockMemoryStore{docs: make(map[string]memory.Document)}
}

// Fetch retrieves a document by path.
func (m *mockMemoryStore) Fetch(_ context.Context, path string) (memory.Document, error) {
	doc, ok := m.docs[path]
	if !ok {
		return memory.Document{}, memory.ErrNotFound
	}
	return doc, nil
}

// Publish creates or updates a document.
func (m *mockMemoryStore) Publish(_ context.Context, path, body string, expectedVersion int) (memory.Document, error) {
	existing, ok := m.docs[path]
	if ok && existing.Version != expectedVersion {
		return memory.Document{}, memory.ErrConflict
	}
	if !ok && expectedVersion != 0 {
		return memory.Document{}, memory.ErrConflict
	}
	ver := expectedVersion + 1
	doc := memory.Document{
		Path:    path,
		Body:    body,
		Version: ver,
	}
	m.docs[path] = doc
	return doc, nil
}

func (m *mockMemoryStore) seed(path, body string, version int) {
	m.docs[path] = memory.Document{
		Path:    path,
		Body:    body,
		Version: version,
	}
}

func TestSetMemoryStore_LoadsWorkTree(t *testing.T) {
	store := newMockMemoryStore()
	store.seed(workTreePath, "---\nproject: TestProject\n---\n# Component\n- [ ] task one\n", 1)

	sess := newWorkTestSession(t)
	sess.SetMemoryStore(store)

	tree := sess.WorkTree()
	if tree == nil {
		t.Fatal("WorkTree returned nil after SetMemoryStore")
	}
	if tree.ProjectName != "TestProject" {
		t.Errorf("ProjectName = %q, want %q", tree.ProjectName, "TestProject")
	}
	if len(tree.Roots) != 1 {
		t.Fatalf("Roots = %d, want 1", len(tree.Roots))
	}
}

func TestSetMemoryStore_NoDocument(t *testing.T) {
	store := newMockMemoryStore()
	sess := newWorkTestSession(t)
	sess.SetMemoryStore(store)

	if tree := sess.WorkTree(); tree != nil {
		t.Errorf("WorkTree = %v, want nil when no project.md exists", tree)
	}
}

func TestSetMemoryStore_Nil(t *testing.T) {
	sess := newWorkTestSession(t)
	sess.SetMemoryStore(nil)
	if tree := sess.WorkTree(); tree != nil {
		t.Errorf("WorkTree = %v, want nil", tree)
	}
}

func TestActiveGoal(t *testing.T) {
	store := newMockMemoryStore()
	store.seed(workTreePath, "# Component\n## Phase\n- [>] active task\n- [ ] other\n", 1)

	sess := newWorkTestSession(t)
	sess.SetMemoryStore(store)

	goal, path := sess.ActiveGoal()
	if goal == nil {
		t.Fatal("ActiveGoal returned nil")
	}
	if goal.Title != "active task" {
		t.Errorf("goal.Title = %q, want %q", goal.Title, "active task")
	}
	if path != "Component > Phase > active task" {
		t.Errorf("path = %q, want %q", path, "Component > Phase > active task")
	}
}

func TestActiveGoal_None(t *testing.T) {
	store := newMockMemoryStore()
	store.seed(workTreePath, "# Component\n- [ ] task\n", 1)

	sess := newWorkTestSession(t)
	sess.SetMemoryStore(store)

	goal, path := sess.ActiveGoal()
	if goal != nil {
		t.Errorf("goal = %v, want nil", goal)
	}
	if path != "" {
		t.Errorf("path = %q, want empty", path)
	}
}

func TestActiveGoal_NoWorkTree(t *testing.T) {
	sess := newWorkTestSession(t)
	goal, path := sess.ActiveGoal()
	if goal != nil {
		t.Errorf("goal = %v, want nil", goal)
	}
	if path != "" {
		t.Errorf("path = %q, want empty", path)
	}
}

func TestSetActiveGoal(t *testing.T) {
	store := newMockMemoryStore()
	store.seed(workTreePath, "# Component\n- [>] old\n- [ ] new target\n", 1)

	sess := newWorkTestSession(t)
	sess.SetMemoryStore(store)

	if err := sess.SetActiveGoal("new target"); err != nil {
		t.Fatal(err)
	}

	goal, _ := sess.ActiveGoal()
	if goal == nil || goal.Title != "new target" {
		t.Errorf("goal = %v, want 'new target'", goal)
	}

	// Verify persisted to store.
	doc, err := store.Fetch(context.Background(), workTreePath)
	if err != nil {
		t.Fatal(err)
	}
	reparsed := project.Parse(doc.Body)
	rGoal, _ := reparsed.ActiveGoal()
	if rGoal == nil || rGoal.Title != "new target" {
		t.Errorf("persisted goal = %v, want 'new target'", rGoal)
	}
}

func TestSetActiveGoal_NotFound(t *testing.T) {
	store := newMockMemoryStore()
	store.seed(workTreePath, "# Component\n- [ ] task\n", 1)

	sess := newWorkTestSession(t)
	sess.SetMemoryStore(store)

	err := sess.SetActiveGoal("nonexistent")
	if err == nil {
		t.Error("SetActiveGoal returned nil error for nonexistent task")
	}
}

func TestSetActiveGoal_NoWorkTree(t *testing.T) {
	sess := newWorkTestSession(t)
	err := sess.SetActiveGoal("anything")
	if err == nil {
		t.Error("SetActiveGoal returned nil error without work tree")
	}
}

func TestSetActiveGoal_ConflictDetection(t *testing.T) {
	store := newMockMemoryStore()
	store.seed(workTreePath, "# A\n- [ ] task\n", 1)

	sess := newWorkTestSession(t)
	sess.SetMemoryStore(store)

	// Simulate external modification — bump version in store.
	store.seed(workTreePath, "# A\n- [ ] task\n- [ ] external addition\n", 5)

	err := sess.SetActiveGoal("task")
	if !errors.Is(err, memory.ErrConflict) {
		t.Errorf("err = %v, want memory.ErrConflict", err)
	}
}

func TestReloadWorkTree(t *testing.T) {
	store := newMockMemoryStore()
	store.seed(workTreePath, "# Original\n", 1)

	sess := newWorkTestSession(t)
	sess.SetMemoryStore(store)

	if sess.WorkTree().Roots[0].Title != "Original" {
		t.Fatal("initial load failed")
	}

	// External update.
	store.seed(workTreePath, "# Updated\n", 2)

	if err := sess.ReloadWorkTree(); err != nil {
		t.Fatal(err)
	}
	if sess.WorkTree().Roots[0].Title != "Updated" {
		t.Errorf("Title = %q, want %q", sess.WorkTree().Roots[0].Title, "Updated")
	}
}

// newWorkTestSession creates a minimal session for testing work tree operations.
// Uses t.TempDir() as the project root to avoid filesystem side effects.
func newWorkTestSession(t *testing.T) *Session {
	t.Helper()
	return New(nil, t.TempDir())
}
