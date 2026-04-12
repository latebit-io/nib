package session

import (
	"github.com/latebit-io/junto/engine/project"
)

// SetMemoryStore injects the memory store for work tree persistence.
// Triggers an initial load of the work tree from demarkus.
func (s *Session) SetMemoryStore(store memoryStore) {
	s.workTree.SetStore(store)
}

// WorkTree returns the current work hierarchy.
func (s *Session) WorkTree() *project.Tree {
	return s.workTree.Tree()
}

// ActiveGoal returns the currently active task and its ancestry path string.
func (s *Session) ActiveGoal() (*project.Node, string) {
	return s.workTree.ActiveGoal()
}

// SetActiveGoal marks the named task as active and persists to demarkus.
func (s *Session) SetActiveGoal(title string) error {
	return s.workTree.SetActiveGoal(title)
}

// MarkGoalDone marks the named task as done and persists to demarkus.
func (s *Session) MarkGoalDone(title string) error {
	return s.workTree.MarkGoalDone(title)
}

// ActivateTask implements agent.TaskTracker.
func (s *Session) ActivateTask(title string) error {
	return s.SetActiveGoal(title)
}

// CompleteTask implements agent.TaskTracker.
func (s *Session) CompleteTask(title string) error {
	return s.MarkGoalDone(title)
}

// ActiveTaskPath implements agent.TaskTracker.
func (s *Session) ActiveTaskPath() string {
	_, path := s.ActiveGoal()
	return path
}

// ReloadWorkTree re-fetches the work tree from demarkus.
func (s *Session) ReloadWorkTree() error {
	return s.workTree.Reload()
}

// FetchWorkTreeSnapshot fetches the work tree without mutating session state.
func (s *Session) FetchWorkTreeSnapshot() WorkTreeSnapshot {
	return s.workTree.FetchSnapshot()
}

// ApplyWorkTreeSnapshot applies a previously fetched snapshot.
func (s *Session) ApplyWorkTreeSnapshot(snap WorkTreeSnapshot) {
	s.workTree.ApplySnapshot(snap)
}
