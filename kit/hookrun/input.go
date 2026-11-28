// Package hookrun runs one Claude Code `command` hook: the event as JSON
// on stdin, a timeout, capped output, and the decision read as Claude
// Code reads it. kit/pluginhooks decides when hooks run.
package hookrun

import "encoding/json"

// Input is the JSON a hook reads on stdin, in Claude Code's shape. Fields
// that do not apply to the event are left out.
type Input struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`

	ToolName  string          `json:"tool_name,omitempty"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	// ToolResponse is the tool's result: its text, for nib's tools.
	ToolResponse any    `json:"tool_response,omitempty"`
	ToolUseID    string `json:"tool_use_id,omitempty"`

	Prompt string `json:"prompt,omitempty"`
	// Source is why a session starts (SessionStart); Reason why it ends (SessionEnd).
	Source string `json:"source,omitempty"`
	Reason string `json:"reason,omitempty"`

	StopHookActive       *bool  `json:"stop_hook_active,omitempty"`
	LastAssistantMessage string `json:"last_assistant_message,omitempty"`
	AgentType            string `json:"agent_type,omitempty"`
}

func (in Input) encode() ([]byte, error) { return json.Marshal(in) }
