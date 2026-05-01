package agent

import (
	upagent "github.com/latebit-io/nib/agent"
	"github.com/latebit-io/nib/coding/tools"
)

// Tool aliases the upstream generic [agent.Tool] interface so the
// rest of this package keeps short, unqualified names. Tool
// implementations live in `coding/tools` and own their own side
// effects through narrow collaborator interfaces (Approver, Navigator,
// FileCreator, TaskReviewer); this package just registers them and
// dispatches.
type Tool = upagent.Tool

// ToolResult aliases the upstream generic [agent.ToolResult]. The
// shape is deliberately minimal — just a string body fed back to the
// LLM and an error flag the frontend can render distinctively.
type ToolResult = upagent.ToolResult

// Resettable aliases the optional reset hook. The agent calls Reset on
// each new run so tools that carry per-run state (silent retry
// counters, last-edit memos) can clear it.
type Resettable = tools.Resettable

// FileReader aliases the workspace read surface tools depend on.
type FileReader = tools.FileReader

// FileWriter aliases the workspace write surface tools depend on.
type FileWriter = tools.FileWriter

// Workspace aliases the full workspace contract (read + write +
// context set) used at the agent boundary.
type Workspace = tools.Workspace

// ContextSet aliases the developer-context surface used by the
// approval flow.
type ContextSet = tools.ContextSet

// TaskReader aliases the read-side task tree surface used by gates and
// next-task hint logic.
type TaskReader = tools.TaskReader

// TaskMutator aliases the write-side task tree surface used by the
// task-related tools.
type TaskMutator = tools.TaskMutator

// TaskTracker aliases the union surface workspaces expose at the agent
// boundary.
type TaskTracker = tools.TaskTracker

// EditProposal aliases the structured payload edit-flow tools build
// and submit through [Approver].
type EditProposal = tools.EditProposal

// FileCache aliases the in-memory file content cache.
type FileCache = tools.FileCache

// NewFileCache constructs an empty [FileCache].
func NewFileCache() *FileCache { return tools.NewFileCache() }
