package protocol

// AgentState represents the current state of the agent loop.
type AgentState string

const (
	StatePlanning    AgentState = "planning"
	StatePending     AgentState = "pending"
	StateApplying    AgentState = "applying"
	StateInterrupted AgentState = "interrupted"
)

// EditOp is a single proposed code change from the agent.
// Line and Col are 1-indexed (matching editor display). The plugin converts
// to 0-indexed when calling Micro's buffer.Loc.
type EditOp struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"` // "insert" | "replace" | "delete"
	Line    int    `json:"line"` // 1-indexed
	Col     int    `json:"col"`  // 1-indexed
	EndLine int    `json:"end_line,omitempty"` // 1-indexed
	EndCol  int    `json:"end_col,omitempty"`  // 1-indexed
	Text    string `json:"text"`
	Reason  string `json:"reason"`
}

// Step is one item in the agent's plan.
type Step struct {
	Description string `json:"description"`
	Done        bool   `json:"done"`
}

// Edit records a change made by either side, kept in a ring buffer.
// Line and Col are 1-indexed.
type Edit struct {
	Source  string `json:"source"` // "agent" | "engineer"
	Line    int    `json:"line"`   // 1-indexed
	Col     int    `json:"col"`    // 1-indexed
	EndLine int    `json:"end_line"` // 1-indexed
	EndCol  int    `json:"end_col"`  // 1-indexed
	Text    string `json:"text"`
}

// WorkingContext is the full state of an active session.
type WorkingContext struct {
	File        string     `json:"file"`
	Plan        []Step     `json:"plan"`
	CurrentStep int        `json:"current_step"`
	State       AgentState `json:"state"`
	PendingOp   *EditOp    `json:"pending_op,omitempty"`
	RecentEdits []Edit     `json:"recent_edits"`
}
