package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/llm"
)

// requestInputOptionIDRE enforces kebab-case on option IDs. The ID flows
// back to the LLM verbatim as the tool-call result when the developer picks
// an option; restricting it to an opaque token (lowercase alphanumeric plus
// single-char hyphens) prevents a malicious or confused model from
// smuggling instructions through what should be a machine identifier while
// hiding the intent behind an innocent-looking label.
var requestInputOptionIDRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,63})$`)

// maxRequestInputOptions caps the number of options a single request_input
// call may supply. Keeps the rendered prompt block compact and forces the
// LLM to collapse nuance into a small set of distinct choices.
const maxRequestInputOptions = 9

// Per-field byte caps on request_input arguments. Bounds LLM-supplied text so
// a single malformed or adversarial tool call cannot balloon the event
// payload, transcript, or status bar. Limits are generous for realistic use
// (e.g. a two-sentence prompt fits easily) but reject obvious runaway sizes.
//
// maxRequestInputArgsBytes caps the raw JSON payload before decoding, so an
// adversarial caller cannot force Unmarshal to allocate megabytes of slice
// or string data before the per-field checks run. Sized with headroom over
// the sum of the per-field caps (prompt + reason + 9 options × (id + label)
// ≈ 5 KB) plus JSON framing overhead.
const (
	maxRequestInputArgsBytes   = 8 * 1024
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
// validateRequestInputOptions checks the caller-supplied option list against
// count, length, format, and uniqueness rules. Returns the validated option
// slice and an empty error string on success, or (nil, "Error: ...") on the
// first validation failure. Extracted from Execute to keep that function's
// cyclomatic complexity under the project cap.
func validateRequestInputOptions(raw []requestInputOptionArg) ([]event.AwaitingInputOption, string) {
	if len(raw) > maxRequestInputOptions {
		return nil, fmt.Sprintf("Error: at most %d options allowed (got %d)", maxRequestInputOptions, len(raw))
	}
	seen := make(map[string]bool, len(raw))
	options := make([]event.AwaitingInputOption, 0, len(raw))
	for i, opt := range raw {
		id := strings.TrimSpace(opt.ID)
		label := strings.TrimSpace(opt.Label)
		if id == "" {
			return nil, fmt.Sprintf("Error: option[%d].id is required", i)
		}
		if label == "" {
			return nil, fmt.Sprintf("Error: option[%d].label is required", i)
		}
		if len(id) > maxRequestInputOptionIDLen {
			return nil, fmt.Sprintf("Error: option[%d].id exceeds %d bytes (got %d)", i, maxRequestInputOptionIDLen, len(id))
		}
		if !requestInputOptionIDRE.MatchString(id) {
			return nil, fmt.Sprintf("Error: option[%d].id %q must be kebab-case (lowercase alphanumeric, hyphens allowed, no leading hyphen)", i, id)
		}
		if len(label) > maxRequestInputLabelBytes {
			return nil, fmt.Sprintf("Error: option[%d].label exceeds %d bytes (got %d)", i, maxRequestInputLabelBytes, len(label))
		}
		if seen[id] {
			return nil, fmt.Sprintf("Error: duplicate option id %q", id)
		}
		seen[id] = true
		options = append(options, event.AwaitingInputOption{ID: id, Label: label})
	}
	return options, ""
}

func (t *RequestInputTool) Execute(ctx context.Context, call llm.ToolCall) ToolResult {
	if ctx.Err() != nil {
		return textResult("Error: agent canceled")
	}
	// Reject oversized raw JSON before Unmarshal — per-field caps below run
	// too late to prevent a pathological payload from allocating megabytes
	// of slice/string data during decoding.
	if len(call.Function.Arguments) > maxRequestInputArgsBytes {
		return textResult(fmt.Sprintf("Error: arguments exceed %d bytes (got %d)", maxRequestInputArgsBytes, len(call.Function.Arguments)))
	}
	var args requestInputArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return textResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	// A non-empty CallID is required: Session tracks the live prompt by
	// CallID and treats empty as "no prompt pending", so an empty ID here
	// would deadlock — the developer's answer would be dropped as a no-op
	// and the agent would stay blocked on the answer channel forever.
	callID := strings.TrimSpace(call.ID)
	if callID == "" {
		return textResult("Error: request_input requires a non-empty tool call ID")
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
	options, errMsg := validateRequestInputOptions(args.Options)
	if errMsg != "" {
		return textResult(errMsg)
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
			CallID:  callID,
		},
	}
}
