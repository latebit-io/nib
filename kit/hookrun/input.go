// Package hookrun executes Claude Code `command` lifecycle hooks: it
// renders the event as JSON on the hook process's stdin, runs the
// command under a timeout with capped output, and parses the process's
// decision (JSON on stdout, falling back to exit-code convention).
//
// It depends on [hookspec] for the parsed hook model but adds the
// impure execution layer kept out of that pure package. Only command
// hooks ([hookspec.Hook.Runnable]) are executed here; the lifecycle
// emit points that decide WHEN to run a hook are a later engine vertical.
package hookrun

import "encoding/json"

// Input is the JSON payload delivered to a hook command on stdin. It
// carries the event and the context a hook typically inspects. Fields are
// omitempty so the payload stays minimal for events that don't populate
// them (e.g. SessionStart has no tool).
type Input struct {
	// Event is the lifecycle event name (a hookspec.Event value).
	Event string `json:"event"`
	// Tool is the tool name for PreToolUse / PostToolUse events.
	Tool string `json:"tool,omitempty"`
	// ToolArgs is the raw tool-call argument JSON, passed through verbatim.
	ToolArgs json.RawMessage `json:"tool_args,omitempty"`
	// ToolResult is the tool's output for PostToolUse.
	ToolResult string `json:"tool_result,omitempty"`
	// Prompt is the user's text for UserPromptSubmit.
	Prompt string `json:"prompt,omitempty"`
	// Cwd is the project working directory.
	Cwd string `json:"cwd,omitempty"`
}

// encode marshals the input for stdin. It never fails for these field
// types, but the error is surfaced for completeness.
func (in Input) encode() ([]byte, error) { return json.Marshal(in) }
