package tools

// TaskReader exposes read-only views over the project's task tree.
type TaskReader interface {
	// ActiveTaskPath returns the ancestry path of the current active
	// task, or empty string if no task is active.
	ActiveTaskPath() string

	// WorkTreeLoaded reports whether the session currently holds a
	// parsed work tree. False means either /project.md does not exist
	// yet or the initial fetch failed (e.g. demarkus unreachable).
	WorkTreeLoaded() bool

	// NextPendingTask returns the title of the first leaf task in
	// document order with status TaskPending, or "" when none exists.
	NextPendingTask() string
}

// TaskMutator covers the write-side operations on the project task
// tree. Tool implementations consume the narrowest mutation surface
// they need (e.g. project_init only requires InitProject).
type TaskMutator interface {
	// ActivateTask marks a task as active in the work tree and persists.
	ActivateTask(title string) error

	// CompleteTask marks a task as done in the work tree and persists.
	CompleteTask(title string) error

	// AddTask appends a new pending task under the given phase and
	// feature, creating the feature if absent, then persists. If link
	// is non-empty, it is appended to the task title as a markdown link.
	AddTask(phase, feature, task, link string) error

	// AddPhase appends a new top-level phase (h1) heading to the work
	// tree and persists, returning the full schema-valid phase title.
	// A bare descriptive title is auto-numbered as "Phase N: Title".
	// Fills the gap between InitProject (seeds phases at bootstrap but
	// is idempotent — refuses to touch an existing plan) and AddTask
	// (creates features under an existing phase, never a new phase).
	AddPhase(title string) (string, error)

	// InitProject ensures /project.md exists with the given project
	// name and h1-level phases, then reloads the work tree so subsequent
	// task operations succeed without manual memory bootstrapping.
	// Idempotent — never overwrites an existing plan.
	InitProject(name string, phases []string) error
}

// TaskTracker is the union surface used at the workspace boundary.
// Tool authors should prefer the narrower [TaskReader] / [TaskMutator]
// interfaces when their tool only needs one half of the contract.
type TaskTracker interface {
	TaskReader
	TaskMutator
}
