package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/latebit-io/junto/engine/memory"
	"github.com/latebit-io/junto/engine/project"
)

// memoryStore is the narrow interface the session needs from the memory system.
// Using a local interface (ISP) keeps session decoupled from the full
// memory.Store — only the operations needed for work tree persistence.
type memoryStore interface {
	Fetch(ctx context.Context, path string) (memory.Document, error)
	Publish(ctx context.Context, path, body string, expectedVersion int) (memory.Document, error)
}

// workTreePath is the demarkus document path for the project work hierarchy.
const workTreePath = "/project.md"

// SetMemoryStore injects the memory store for work tree persistence.
// Triggers an initial load of the work tree from demarkus.
// Safe to call with nil (disables work tree features).
func (s *Session) SetMemoryStore(store memoryStore) {
	s.memoryStore = store

	// Clear work-tree state before (re)loading to avoid stale data
	// when the store changes or is set to nil.
	s.workTree = nil
	s.workTreeVer = 0
	s.workTreeDirty = false

	if store == nil {
		return
	}
	if err := s.loadWorkTree(); err != nil {
		slog.Warn("session: failed to load work tree", "err", err)
	}
}

// WorkTree returns the current work hierarchy. May be nil if no project.md
// exists in demarkus or memory is not configured.
func (s *Session) WorkTree() *project.Tree {
	return s.workTree
}

// ActiveGoal returns the currently active task and its ancestry path string.
// Returns nil, "" if no work tree is loaded or no active goal is set.
func (s *Session) ActiveGoal() (*project.Node, string) {
	if s.workTree == nil {
		return nil, ""
	}
	goal, ancestry := s.workTree.ActiveGoal()
	if goal == nil {
		return nil, ""
	}
	parts := make([]string, len(ancestry))
	for i, n := range ancestry {
		parts[i] = n.Title
	}
	return goal, strings.Join(parts, " > ")
}

// SetActiveGoal marks the named task as active in the work tree and persists
// the change to demarkus. Returns an error if the target is not found,
// the work tree is not loaded, or persistence fails.
func (s *Session) SetActiveGoal(title string) error {
	if s.workTree == nil {
		return errors.New("session: no work tree loaded")
	}
	if !s.workTree.SetActiveGoal(title) {
		return fmt.Errorf("session: task not found: %q", title)
	}
	s.workTreeDirty = true
	return s.saveWorkTree()
}

// MarkGoalDone marks the named task as done in the work tree and persists
// the change to demarkus. Returns an error if the target is not found,
// the work tree is not loaded, or persistence fails.
func (s *Session) MarkGoalDone(title string) error {
	if s.workTree == nil {
		return errors.New("session: no work tree loaded")
	}
	if !s.workTree.MarkDone(title) {
		return fmt.Errorf("session: task not found: %q", title)
	}
	s.workTreeDirty = true
	return s.saveWorkTree()
}

// ActivateTask implements agent.TaskTracker. Marks a task as active in the
// work tree and persists to demarkus.
func (s *Session) ActivateTask(title string) error {
	return s.SetActiveGoal(title)
}

// CompleteTask implements agent.TaskTracker. Marks a task as done in the
// work tree and persists to demarkus.
func (s *Session) CompleteTask(title string) error {
	return s.MarkGoalDone(title)
}

// ActiveTaskPath implements agent.TaskTracker. Returns the ancestry path
// of the current active task.
func (s *Session) ActiveTaskPath() string {
	_, path := s.ActiveGoal()
	return path
}

// ReloadWorkTree re-fetches the work tree from demarkus, discarding any
// unsaved local changes. Useful after external modifications to project.md.
// Must be called from the TUI goroutine — mutates session state directly.
func (s *Session) ReloadWorkTree() error {
	return s.loadWorkTree()
}

// WorkTreeSnapshot holds the result of a background work tree fetch.
// Used to separate I/O (safe in any goroutine) from state mutation
// (must happen on the TUI goroutine).
type WorkTreeSnapshot struct {
	Tree    *project.Tree
	Version int
	Err     error
}

// FetchWorkTreeSnapshot fetches the work tree from demarkus without mutating
// session state. Safe to call from any goroutine (e.g. a tea.Cmd).
// Apply the result via ApplyWorkTreeSnapshot on the TUI goroutine.
func (s *Session) FetchWorkTreeSnapshot() WorkTreeSnapshot {
	if s.memoryStore == nil {
		return WorkTreeSnapshot{}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	doc, err := s.memoryStore.Fetch(ctx, workTreePath)
	if errors.Is(err, memory.ErrNotFound) {
		return WorkTreeSnapshot{} // no document — tree is nil, not an error
	}
	if err != nil {
		return WorkTreeSnapshot{Err: err}
	}
	return WorkTreeSnapshot{
		Tree:    project.Parse(doc.Body),
		Version: doc.Version,
	}
}

// ApplyWorkTreeSnapshot applies a previously fetched snapshot to the session.
// Must be called from the TUI goroutine — mutates session state directly.
func (s *Session) ApplyWorkTreeSnapshot(snap WorkTreeSnapshot) {
	s.workTree = snap.Tree
	s.workTreeVer = snap.Version
	s.workTreeDirty = false
}

// loadWorkTree fetches project.md from demarkus and parses it into the
// work tree. Sets workTree to nil if the document does not exist.
func (s *Session) loadWorkTree() error {
	if s.memoryStore == nil {
		return errors.New("no memory store configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	doc, err := s.memoryStore.Fetch(ctx, workTreePath)
	if errors.Is(err, memory.ErrNotFound) {
		s.workTree = nil
		s.workTreeVer = 0
		s.workTreeDirty = false
		return nil
	}
	if err != nil {
		return err
	}

	s.workTree = project.Parse(doc.Body)
	s.workTreeVer = doc.Version
	s.workTreeDirty = false
	return nil
}

// saveWorkTree serializes the current work tree and publishes it to demarkus.
// Uses optimistic concurrency — fails with memory.ErrConflict if the document
// was modified externally since the last fetch.
func (s *Session) saveWorkTree() error {
	if s.memoryStore == nil {
		return errors.New("no memory store configured")
	}
	if s.workTree == nil {
		return errors.New("no work tree to save")
	}

	body := project.Serialize(s.workTree)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	doc, err := s.memoryStore.Publish(ctx, workTreePath, body, s.workTreeVer)
	if err != nil {
		return err
	}
	s.workTreeVer = doc.Version
	s.workTreeDirty = false
	return nil
}
