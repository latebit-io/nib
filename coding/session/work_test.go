package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/latebit-io/nib/engine/project"
	"github.com/latebit-io/nib/kit/memory"
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

func TestSetMemoryStore_NilClearsStaleState(t *testing.T) {
	store := newMockMemoryStore()
	store.seed(workTreePath, "# Component\n- [>] active\n", 1)

	sess := newWorkTestSession(t)
	sess.SetMemoryStore(store)

	if sess.WorkTree() == nil {
		t.Fatal("WorkTree should be loaded")
	}

	// Setting nil must clear the previously loaded tree.
	sess.SetMemoryStore(nil)

	if sess.WorkTree() != nil {
		t.Error("WorkTree should be nil after SetMemoryStore(nil)")
	}
	goal, path := sess.ActiveGoal()
	if goal != nil || path != "" {
		t.Errorf("ActiveGoal should return nil, empty after nil store; got %v, %q", goal, path)
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

func TestMarkGoalDone(t *testing.T) {
	store := newMockMemoryStore()
	store.seed(workTreePath, "# A\n- [>] active task\n- [ ] other\n", 1)

	sess := newWorkTestSession(t)
	sess.SetMemoryStore(store)

	if err := sess.MarkGoalDone("active task"); err != nil {
		t.Fatal(err)
	}

	// Verify in-memory state.
	tree := sess.WorkTree()
	if tree.Roots[0].Children[0].Status != project.TaskDone {
		t.Errorf("status = %v, want TaskDone", tree.Roots[0].Children[0].Status)
	}

	// Verify persisted.
	doc, err := store.Fetch(context.Background(), workTreePath)
	if err != nil {
		t.Fatal(err)
	}
	reparsed := project.Parse(doc.Body)
	if reparsed.Roots[0].Children[0].Status != project.TaskDone {
		t.Errorf("persisted status = %v, want TaskDone", reparsed.Roots[0].Children[0].Status)
	}
}

func TestAddPhase(t *testing.T) {
	store := newMockMemoryStore()
	store.seed(workTreePath, "---\nproject: P\n---\n# Phase 1: Foundation\n## F\n- [ ] t\n", 1)

	sess := newWorkTestSession(t)
	sess.SetMemoryStore(store)

	full, err := sess.AddPhase("Polish")
	if err != nil {
		t.Fatalf("AddPhase: %v", err)
	}
	if full != "Phase 2: Polish" {
		t.Errorf("full = %q, want %q", full, "Phase 2: Polish")
	}

	// Verify in-memory state gained the phase.
	tree := sess.WorkTree()
	if len(tree.Roots) != 2 || tree.Roots[1].Title != "Phase 2: Polish" {
		t.Fatalf("Roots = %+v, want appended 'Phase 2: Polish'", tree.Roots)
	}

	// Verify persisted and schema-valid on round-trip.
	doc, err := store.Fetch(context.Background(), workTreePath)
	if err != nil {
		t.Fatal(err)
	}
	reparsed := project.Parse(doc.Body)
	if len(reparsed.Roots) != 2 || reparsed.Roots[1].Title != "Phase 2: Polish" {
		t.Errorf("persisted Roots = %+v, want appended phase", reparsed.Roots)
	}
	if errs := project.Validate(reparsed); errs != nil {
		t.Errorf("persisted tree must validate: %v", errs)
	}
}

func TestAddPhase_NoWorkTree(t *testing.T) {
	sess := newWorkTestSession(t)
	if _, err := sess.AddPhase("Polish"); err == nil {
		t.Error("AddPhase returned nil error without work tree")
	}
}

func TestMarkGoalDone_NotFound(t *testing.T) {
	store := newMockMemoryStore()
	store.seed(workTreePath, "# A\n- [ ] task\n", 1)

	sess := newWorkTestSession(t)
	sess.SetMemoryStore(store)

	err := sess.MarkGoalDone("nonexistent")
	if err == nil {
		t.Error("MarkGoalDone returned nil error for nonexistent task")
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

// TestBuildProjectSkeleton_SanitisesNameAndPhases verifies that LLM-
// supplied name/phase strings containing newlines or excessive
// whitespace are collapsed to a single line. Without this, a
// multi-line name could close the `---` frontmatter early or inject
// phantom YAML keys, and a multi-line phase title could create
// unintended phases or task lines — the project parser is a naive
// line splitter and would happily mis-parse the corruption.
func TestBuildProjectSkeleton_SanitisesNameAndPhases(t *testing.T) {
	t.Parallel()

	// Name with embedded newline + a phase with a newline that would
	// otherwise become a second `# Heading` in the rendered doc.
	doc := buildProjectSkeleton(
		"My\n---\nproject: hijacked\nProject",
		[]string{"Foundation\n# Smuggled Phase\n- [>] smuggled task"},
	)

	tree := project.Parse(doc)
	if tree.ProjectName != "My --- project: hijacked Project" {
		t.Errorf("ProjectName = %q; want collapsed single-line value", tree.ProjectName)
	}
	if len(tree.Roots) != 1 {
		t.Fatalf("Roots = %d; want exactly 1 (no smuggled headings): %s", len(tree.Roots), doc)
	}
	if !strings.Contains(tree.Roots[0].Title, "Foundation") {
		t.Errorf("Phase title = %q; want sanitised Foundation prefix", tree.Roots[0].Title)
	}
	if strings.Contains(tree.Roots[0].Title, "\n") {
		t.Errorf("Phase title must not contain newline: %q", tree.Roots[0].Title)
	}
	if len(tree.Roots[0].Children) != 0 {
		t.Errorf("phase has %d unexpected children — newline injection may have created phantom tasks: %+v",
			len(tree.Roots[0].Children), tree.Roots[0].Children)
	}
}

// TestBuildProjectSkeleton_CapsPhaseCount verifies the phase count
// cap kicks in for absurdly large LLM input. Without the cap, sloppy
// agent output could write a multi-MB /project.md before the
// demarkus body-size check rejects it.
func TestBuildProjectSkeleton_CapsPhaseCount(t *testing.T) {
	t.Parallel()

	phases := make([]string, maxProjectInitPhases+50)
	for i := range phases {
		phases[i] = fmt.Sprintf("Phase %d", i)
	}
	doc := buildProjectSkeleton("Big", phases)
	tree := project.Parse(doc)
	if len(tree.Roots) != maxProjectInitPhases {
		t.Errorf("Roots = %d; want cap %d (extra phases must be truncated)",
			len(tree.Roots), maxProjectInitPhases)
	}
}
