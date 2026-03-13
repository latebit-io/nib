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
type EditOp struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"` // "insert" | "replace" | "delete"
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	EndLine int    `json:"end_line,omitempty"`
	EndCol  int    `json:"end_col,omitempty"`
	Text    string `json:"text"`
	Reason  string `json:"reason"`
}

// Step is one item in the agent's plan.
type Step struct {
	Description string `json:"description"`
	Done        bool   `json:"done"`
}

// Edit records a change made by either side, kept in a ring buffer.
type Edit struct {
	Source  string `json:"source"` // "agent" | "engineer"
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	EndLine int    `json:"end_line"`
	EndCol  int    `json:"end_col"`
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
