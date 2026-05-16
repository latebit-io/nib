// Package tools holds the application-layer tool implementations the
// agent dispatches against. Each file in this package is an agent
// tool (read_file, edit_file, …) that satisfies the generic
// [agent.Tool] interface defined in the upstream `agent` module.
//
// Tools own their side effects directly: they are constructed with
// narrow collaborator interfaces (Approver, Navigator, FileCreator,
// TaskReviewer) that the application layer in `coding/agent` provides.
// The agent loop never inspects the tool's return shape beyond passing
// the [agent.ToolResult] body to the LLM and the IsError flag to the
// frontend.
//
// This package is the home of the workspace-side contracts (FileReader,
// FileWriter, Workspace, ContextSet, TaskReader, TaskMutator,
// TaskTracker), the [FileCache] used by file-touching tools, and the
// [EditProposal] payload that flows from edit/replace tools through
// [Approver] to the approval pipeline.
package tools
