package session

import "fmt"

// AutonomyLevel controls how much the agent acts without explicit developer approval.
// Higher levels reduce interruptions; lower levels maximise developer oversight.
//
// The type lives in the engine (not a frontend) because the agent loop consults
// it when deciding whether to run silent validator retries versus surfacing a
// failure to the developer. Frontends read and mutate the value; the engine
// enforces the semantics.
type AutonomyLevel int

const (
	// LevelGuided requires explicit approval for every proposed edit (Ctrl+O).
	LevelGuided AutonomyLevel = 1

	// LevelTrusted applies proposed edits instantly without requiring approval.
	// The developer can still cancel an in-progress run with Escape. Validator
	// Block verdicts (the safety valve for "this edit is structurally wrong")
	// still surface the diff for review — auto-approval is for routine work,
	// not for edits the validators flagged as needing developer attention.
	LevelTrusted AutonomyLevel = 2

	// LevelYolo extends LevelTrusted by ALSO auto-applying edits whose
	// validator summaries carry a Block verdict. This is an opt-in escape
	// hatch for benchmark / exploratory runs where the developer has
	// accepted the risk that architecture caps and similar guards may be
	// violated, and would rather see the result than be interrupted.
	//
	// Use deliberately. The Block verdict exists because at least one
	// validator stage decided the edit needed eyes — auto-applying it
	// means the developer is responsible for catching the consequences
	// (oversized files, broken structure) at PR review or after the
	// fact rather than at edit time. The autonomy dial defaults to
	// LevelTrusted on every TUI startup; LevelYolo never persists.
	LevelYolo AutonomyLevel = 3
)

// Name returns the short display name for the autonomy level.
func (l AutonomyLevel) Name() string {
	switch l {
	case LevelGuided:
		return "guided"
	case LevelTrusted:
		return "trust"
	case LevelYolo:
		return "yolo"
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
	if l >= LevelYolo {
		return LevelGuided
	}
	return l + 1
}

// AutoApproveEdits reports whether proposed edits should be applied without
// requiring explicit developer approval (Ctrl+O).
func (l AutonomyLevel) AutoApproveEdits() bool { return l >= LevelTrusted }

// AutoApproveBlock reports whether proposed edits whose validator
// summaries carry a Block verdict should ALSO be auto-applied. Only
// LevelYolo opts into this — under LevelTrusted the safety valve
// holds (Block surfaces the diff for explicit Ctrl+O / Esc).
func (l AutonomyLevel) AutoApproveBlock() bool { return l >= LevelYolo }
