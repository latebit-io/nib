package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/junto/engine/memory"
	"github.com/latebit-io/junto/engine/project"
)

// memoryStore is the narrow interface needed from the memory system.
// Using a local interface (ISP) keeps session decoupled from the full
// memory.Store — only the operations needed for work tree persistence.
type memoryStore interface {
	Fetch(ctx context.Context, path string) (memory.Document, error)
	Publish(ctx context.Context, path, body string, expectedVersion int) (memory.Document, error)
}

// workTreePath is the demarkus document path for the project work hierarchy.
const workTreePath = "/project.md"

// WorkTreeManager manages the structured project hierarchy loaded from
// demarkus memory. Extracted from Session to give the work tree its own
// lock and testable boundary.
type WorkTreeManager struct {
	mu     sync.RWMutex
	store  memoryStore
	tree   *project.Tree
	ver    int
	dirty  bool
	modGen uint64
}

// SetStore injects the memory store for work tree persistence.
// Triggers an initial load of the work tree from demarkus.
// Safe to call with nil (disables work tree features).
func (w *WorkTreeManager) SetStore(store memoryStore) {
	w.mu.Lock()
	w.store = store
	w.tree = nil
	w.ver = 0
	w.dirty = false
	w.mu.Unlock()

	if store == nil {
		return
	}
	if err := w.load(); err != nil {
		// Log but don't fail startup — work tree is optional. tree=nil is
		// a valid state; mutation methods (SetActiveGoal, etc.) return
		// "not loaded" errors if the tree is absent.
		slog.Warn("work tree: initial load failed", "err", err)
	}
}

// Tree returns the current work hierarchy. May be nil if no project.md
// exists in demarkus or memory is not configured.
func (w *WorkTreeManager) Tree() *project.Tree {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.tree
}

// ActiveGoal returns the currently active task and its ancestry path string.
// Returns nil, "" if no work tree is loaded or no active goal is set.
func (w *WorkTreeManager) ActiveGoal() (*project.Node, string) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.activeGoalLocked()
}

// activeGoalLocked is the lock-free inner implementation of ActiveGoal.
// Caller must hold at least mu.RLock.
func (w *WorkTreeManager) activeGoalLocked() (*project.Node, string) {
	if w.tree == nil {
		return nil, ""
	}
	goal, ancestry := w.tree.ActiveGoal()
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
func (w *WorkTreeManager) SetActiveGoal(title string) error {
	w.mu.Lock()
	if w.tree == nil {
		w.mu.Unlock()
		return errors.New("work tree: not loaded")
	}
	if !w.tree.SetActiveGoal(title) {
		w.mu.Unlock()
		return errors.New("work tree: task not found: " + title)
	}
	w.dirty = true
	w.modGen++
	return w.saveAndUnlock()
}

// AddTask appends a new pending task to the work tree under the named
// phase and feature, creating the feature if absent, and persists the
// change to demarkus. Returns an error if the work tree is not loaded,
// the phase is not found, or persistence fails. If link is non-empty,
// it is appended to the task title as a markdown link to a supplementary
// memory document.
func (w *WorkTreeManager) AddTask(phase, feature, task, link string) error {
	w.mu.Lock()
	if w.tree == nil {
		w.mu.Unlock()
		return errors.New("work tree: not loaded")
	}
	if err := w.tree.AddTask(phase, feature, task, link); err != nil {
		w.mu.Unlock()
		return fmt.Errorf("work tree: %w", err)
	}
	w.dirty = true
	w.modGen++
	return w.saveAndUnlock()
}

// MarkGoalDone marks the named task as done in the work tree and persists
// the change to demarkus. Returns an error if the target is not found,
// the work tree is not loaded, or persistence fails.
func (w *WorkTreeManager) MarkGoalDone(title string) error {
	w.mu.Lock()
	if w.tree == nil {
		w.mu.Unlock()
		return errors.New("work tree: not loaded")
	}
	if !w.tree.MarkDone(title) {
		w.mu.Unlock()
		return errors.New("work tree: task not found: " + title)
	}
	w.dirty = true
	w.modGen++
	return w.saveAndUnlock()
}

// Reload re-fetches the work tree from demarkus, discarding any
// unsaved local changes.
func (w *WorkTreeManager) Reload() error {
	return w.load()
}

// WorkTreeSnapshot holds the result of a background work tree fetch.
type WorkTreeSnapshot struct {
	Tree    *project.Tree
	Version int
	Err     error
}

// FetchSnapshot fetches the work tree from demarkus without mutating state.
// Safe to call from any goroutine. Apply the result via ApplySnapshot.
func (w *WorkTreeManager) FetchSnapshot() WorkTreeSnapshot {
	w.mu.RLock()
	store := w.store
	w.mu.RUnlock()

	if store == nil {
		return WorkTreeSnapshot{}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	doc, err := store.Fetch(ctx, workTreePath)
	if errors.Is(err, memory.ErrNotFound) {
		return WorkTreeSnapshot{}
	}
	if err != nil {
		return WorkTreeSnapshot{Err: err}
	}
	return WorkTreeSnapshot{
		Tree:    project.Parse(doc.Body),
		Version: doc.Version,
	}
}

// ApplySnapshot applies a previously fetched snapshot, unless local
// modifications occurred between the fetch and apply (detected via dirty
// flag) or a newer version was loaded since the fetch (version guard).
// Returns true if the snapshot was applied, false if skipped.
func (w *WorkTreeManager) ApplySnapshot(snap WorkTreeSnapshot) bool {
	if snap.Err != nil {
		// Failed fetch — never overwrite current state with error result.
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dirty {
		// Local mutation happened since the fetch — don't clobber it.
		return false
	}
	if snap.Version > 0 && snap.Version <= w.ver {
		// Stale snapshot — a newer version was loaded since fetch.
		return false
	}
	w.tree = snap.Tree
	w.ver = snap.Version
	w.dirty = false
	return true
}

// load fetches project.md from demarkus and parses it into the work tree.
// Note: there is a small TOCTOU window between capturing store (under RLock)
// and updating state (under Lock). In practice SetStore is called once at
// session init, so a concurrent store swap cannot happen.
func (w *WorkTreeManager) load() error {
	w.mu.RLock()
	store := w.store
	w.mu.RUnlock()

	if store == nil {
		return errors.New("no memory store configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	doc, err := store.Fetch(ctx, workTreePath)

	w.mu.Lock()
	defer w.mu.Unlock()

	if errors.Is(err, memory.ErrNotFound) {
		w.tree = nil
		w.ver = 0
		w.dirty = false
		return nil
	}
	if err != nil {
		return err
	}

	w.tree = project.Parse(doc.Body)
	w.ver = doc.Version
	w.dirty = false
	return nil
}

// saveAndUnlock snapshots the work tree under lock, unlocks, performs I/O,
// then re-locks to update the version. Caller must hold mu.Lock on entry;
// the lock is always released by the time this method returns.
func (w *WorkTreeManager) saveAndUnlock() error {
	store := w.store
	if store == nil {
		w.mu.Unlock()
		return errors.New("no memory store configured")
	}
	if w.tree == nil {
		w.mu.Unlock()
		return errors.New("no work tree to save")
	}

	body := project.Serialize(w.tree)
	ver := w.ver
	genBefore := w.modGen
	w.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	doc, err := store.Publish(ctx, workTreePath, body, ver)
	if err != nil {
		return err
	}

	w.mu.Lock()
	if w.ver == ver {
		w.ver = doc.Version
	}
	if w.modGen == genBefore {
		w.dirty = false
	}
	w.mu.Unlock()
	return nil
}
