package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/llm"
)

// maxRequestInputOptions caps the number of options a single request_input
// call may supply. Keeps the rendered prompt block compact and forces the
// LLM to collapse nuance into a small set of distinct choices.
const maxRequestInputOptions = 9

// Per-field byte caps on request_input arguments. Bounds LLM-supplied text so
// a single malformed or adversarial tool call cannot balloon the event
// payload, transcript, or status bar. Limits are generous for realistic use
// (e.g. a two-sentence prompt fits easily) but reject obvious runaway sizes.
const (
	maxRequestInputPromptBytes = 2000
	maxRequestInputReasonBytes = 500
	maxRequestInputOptionIDLen = 64
	maxRequestInputLabelBytes  = 200
)

// RequestInputTool lets the agent pause mid-turn and ask the developer a
// structured question. The TUI renders a distinct prompt block so the
// developer can see at a glance that the agent is blocked on a decision
// rather than parsing prose for question marks.
//
// Not registered in headless interaction mode — headless agents must
// decide autonomously without asking.
type RequestInputTool struct{}

// NewRequestInputTool creates a RequestInputTool. It has no dependencies;
// all orchestration happens in the agent loop via EffectAwaitingInput.
func NewRequestInputTool() *RequestInputTool {
	return &RequestInputTool{}
}

type requestInputOptionArg struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type requestInputArgs struct {
	Prompt  string                  `json:"prompt"`
	Options []requestInputOptionArg `json:"options,omitempty"`
	Reason  string                  `json:"reason,omitempty"`
}

// Definition returns the tool schema for the LLM.
func (t *RequestInputTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name: "request_input",
			Description: "Pause and ask the developer a question when your next action depends on a choice only they can make. " +
				"Use this instead of asking in prose — the TUI renders a distinct prompt block so the developer cannot miss it. " +
				"Supply discrete option IDs when you can; omit options for open-ended questions. " +
				"Do NOT use this for edit approval (that has its own flow) or for questions you can answer by reading the code.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"prompt": {
						Type:        "string",
						Description: "The question to show the developer. One or two short sentences.",
					},
					"options": {
						Type:        "array",
						Description: "Suggested choices. Short kebab-case IDs, human-readable labels. 0–9 allowed. Omit for free-form questions.",
					},
					"reason": {
						Type:        "string",
						Description: "Optional one-line explanation of why input is needed.",
					},
				},
				Required: []string{"prompt"},
			},
		},
	}
}

// Execute validates the args and returns EffectAwaitingInput so the agent
// loop can emit AgentAwaitingInput and block on the answer channel.
func (t *RequestInputTool) Execute(ctx context.Context, call llm.ToolCall) ToolResult {
	if ctx.Err() != nil {
		return textResult("Error: agent canceled")
	}
	var args requestInputArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return textResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	prompt := strings.TrimSpace(args.Prompt)
	if prompt == "" {
		return textResult("Error: prompt is required and must not be empty")
	}
	if len(prompt) > maxRequestInputPromptBytes {
		return textResult(fmt.Sprintf("Error: prompt exceeds %d bytes (got %d)", maxRequestInputPromptBytes, len(prompt)))
	}
	reason := strings.TrimSpace(args.Reason)
	if len(reason) > maxRequestInputReasonBytes {
		return textResult(fmt.Sprintf("Error: reason exceeds %d bytes (got %d)", maxRequestInputReasonBytes, len(reason)))
	}
	if len(args.Options) > maxRequestInputOptions {
		return textResult(fmt.Sprintf("Error: at most %d options allowed (got %d)", maxRequestInputOptions, len(args.Options)))
	}

	seen := make(map[string]bool, len(args.Options))
	options := make([]event.AwaitingInputOption, 0, len(args.Options))
	for i, opt := range args.Options {
		id := strings.TrimSpace(opt.ID)
		label := strings.TrimSpace(opt.Label)
		if id == "" {
			return textResult(fmt.Sprintf("Error: option[%d].id is required", i))
		}
		if label == "" {
			return textResult(fmt.Sprintf("Error: option[%d].label is required", i))
		}
		if len(id) > maxRequestInputOptionIDLen {
			return textResult(fmt.Sprintf("Error: option[%d].id exceeds %d bytes (got %d)", i, maxRequestInputOptionIDLen, len(id)))
		}
		if len(label) > maxRequestInputLabelBytes {
			return textResult(fmt.Sprintf("Error: option[%d].label exceeds %d bytes (got %d)", i, maxRequestInputLabelBytes, len(label)))
		}
		if seen[id] {
			return textResult(fmt.Sprintf("Error: duplicate option id %q", id))
		}
		seen[id] = true
		options = append(options, event.AwaitingInputOption{ID: id, Label: label})
	}

	return ToolResult{
		// Content is overwritten by the agent loop with the developer's
		// answer; seed it so an accidental read before the block is safe.
		Content: "",
		Effect:  EffectAwaitingInput,
		Payload: AwaitingInputPayload{
			Prompt:  prompt,
			Options: options,
			Reason:  reason,
			CallID:  call.ID,
		},
	}
}
