package ui

import "fmt"

// AutonomyLevel controls how much the agent acts without explicit developer approval.
// Higher levels reduce interruptions; lower levels maximise developer oversight.
type AutonomyLevel int

const (
	// LevelGuided requires explicit approval for every proposed edit (Ctrl+O)
	// and an explicit continue signal after each edit (Ctrl+N).
	LevelGuided AutonomyLevel = 1

	// LevelCollaborate requires explicit approval for each proposed edit (Ctrl+O)
	// but continues the agent automatically after the animation completes,
	// removing the need to press Ctrl+N after every edit.
	LevelCollaborate AutonomyLevel = 2

	// LevelTrusted applies proposed edits instantly without requiring approval
	// or animation, and continues the agent automatically. The developer can
	// still cancel an in-progress run with Escape.
	LevelTrusted AutonomyLevel = 3
)

// Name returns the short display name for the autonomy level.
func (l AutonomyLevel) Name() string {
	switch l {
	case LevelGuided:
		return "guided"
	case LevelCollaborate:
		return "collaborate"
	case LevelTrusted:
		return "trust"
	default:
		return "unknown"
	}
}

// String returns the status-bar label shown to the developer.
func (l AutonomyLevel) String() string {
	return fmt.Sprintf("dial:%d·%s", int(l), l.Name())
}

// Cycle advances to the next level, wrapping from the highest back to LevelGuided.
func (l AutonomyLevel) Cycle() AutonomyLevel {
	if l >= LevelTrusted {
		return LevelGuided
	}
	return l + 1
}

// AutoApproveEdits reports whether proposed edits should be applied without
// requiring explicit developer approval (Ctrl+O).
func (l AutonomyLevel) AutoApproveEdits() bool { return l >= LevelTrusted }

// AutoContinue reports whether the agent should continue automatically after
// an approved edit without requiring an explicit Ctrl+N.
func (l AutonomyLevel) AutoContinue() bool { return l >= LevelCollaborate }
