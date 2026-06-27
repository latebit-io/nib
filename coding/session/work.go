package session

import (
	"log/slog"

	"github.com/latebit-io/nib/engine/project"
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

// AddTask implements agent.TaskTracker.
func (s *Session) AddTask(phase, feature, task, link string) error {
	return s.workTree.AddTask(phase, feature, task, link)
}

// AddPhase implements agent.TaskTracker. Appends a new top-level phase
// to /project.md and persists, returning the full (auto-numbered)
// phase title so the caller can target it with project_task_add.
func (s *Session) AddPhase(title string) (string, error) {
	return s.workTree.AddPhase(title)
}

// ActiveTaskPath implements agent.TaskTracker.
func (s *Session) ActiveTaskPath() string {
	_, path := s.ActiveGoal()
	return path
}

// WorkTreeLoaded implements agent.TaskTracker.
func (s *Session) WorkTreeLoaded() bool {
	return s.workTree.TreeLoaded()
}

// InitProject implements agent.TaskTracker. Bootstraps /project.md
// with the given name and h1-phase headings, then reloads the work
// tree so subsequent task operations succeed. Idempotent — never
// overwrites an existing plan; if /project.md already exists, this
// just refreshes the in-memory tree.
//
// Distinct from memory_publish (which is for session memory and
// arbitrary docs) — this tool is the only sanctioned path for
// creating the project's task-tracking document.
func (s *Session) InitProject(name string, phases []string) error {
	return s.workTree.InitProject(name, phases)
}

// NextPendingTask implements agent.TaskTracker. Returns the title of
// the first leaf task in document order with status TaskPending, or
// "" when none remains. The agent's runTaskReview hook appends this
// to the task-completion review so the LLM has a clear next step
// without the developer prompting between every task.
func (s *Session) NextPendingTask() string {
	t := s.WorkTree()
	if t == nil {
		return ""
	}
	return t.FindNextPendingTask()
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
// Skipped if local modifications occurred between the fetch and apply.
func (s *Session) ApplyWorkTreeSnapshot(snap WorkTreeSnapshot) {
	if !s.workTree.ApplySnapshot(snap) {
		slog.Debug("session: skipped stale work tree snapshot (local modifications pending)")
	}
}
