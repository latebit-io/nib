package editor

// AnimatedEdit is the interface for engine-side animated edits.
// IncrementalEdit implements this interface. Surgical animation uses
// the same IncrementalEdit but with narrowed search/replace spans
// computed via NarrowEdit — unchanged prefix/suffix lines are excluded.
// Frontends consume this interface to drive animated edit playback.
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
