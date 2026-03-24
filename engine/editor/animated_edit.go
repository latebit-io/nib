package editor

// AnimatedEdit is the interface for engine-side animated edits.
// Both IncrementalEdit (monolithic delete+retype) and SurgicalEdit
// (hunk-by-hunk with unchanged lines preserved) implement it.
// The TUI animation loop consumes this interface — it does not need
// to know which implementation is behind it.
//
//nolint:interfacebloat // animation lifecycle requires all 8 methods — advance, position (2), lifecycle (3), progress, yield
type AnimatedEdit interface {
	// Advance inserts up to charsPerTick characters. Returns what happened.
	Advance() AdvanceResult

	// Position returns the current insert position (agent cursor location).
	Position() (line, col int)

	// StartPosition returns where the edit region begins (for collision detection).
	StartPosition() (line, col int)

	// Complete closes the undo group. The edit is final and undoable as one unit.
	Complete()

	// Abort closes the undo group without finishing. Partial edit stays, undoable.
	Abort()

	// Remaining returns the number of replacement runes not yet inserted.
	Remaining() int

	// FinishLine inserts characters up to the next newline (clean boundary yield).
	FinishLine()

	// IsComplete returns true if Complete or Abort has been called.
	IsComplete() bool
}

// Compile-time interface satisfaction checks.
var (
	_ AnimatedEdit = (*IncrementalEdit)(nil)
)
