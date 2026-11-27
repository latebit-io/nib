package llm

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
)

// ResponsesAPI implements Provider for an Open Responses endpoint
// (openresponses.org): the Responses API wire format, at the base URL's
// /responses. [CodexAPI] speaks the same format to the ChatGPT backend.
type ResponsesAPI struct{ *responses }

var _ OutputCapEscalator = (*ResponsesAPI)(nil)

// responses is one Responses endpoint, shared by the providers that
// speak the format; they differ in endpoint, label and defaults.
type responses struct {
	auth     Auth
	endpoint string
	provider string // labels errors and [AuthError.Provider]
	model    string
	effort   Effort // reasoning effort; "" omits the reasoning field
	client   *http.Client
	// instructions is sent when a conversation has no system message.
	instructions string

	// maxTokensState's zero value omits max_output_tokens so the
	// provider default applies until the agent escalates.
	maxTokensState
}

// NewResponsesAPI creates a provider for the Open Responses endpoint at
// baseURL + "/responses" (baseURL like "https://api.example.com/v1").
// effort sets the reasoning effort ("" for the provider default).
func NewResponsesAPI(baseURL, model string, auth Auth, effort Effort) *ResponsesAPI {
	return &ResponsesAPI{newResponses(strings.TrimRight(baseURL, "/")+"/responses", "responses", model, auth, effort)}
}

func newResponses(endpoint, provider, model string, auth Auth, effort Effort) *responses {
	return &responses{auth: auth, endpoint: endpoint, provider: provider, model: model, effort: effort,
		client: &http.Client{Transport: agentTransport()}}
}

// Stream sends a request to the endpoint and returns a channel of
// streaming events. Nothing is stored on the server: every request
// carries the whole conversation.
func (r *responses) Stream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamEvent, error) {
	instructions, input := messagesToResponsesInput(messages)
	request := responsesRequest{
		Model:           r.model,
		Instructions:    cmp.Or(instructions, r.instructions),
		Input:           input,
		Tools:           toolsToResponsesTools(tools),
		Stream:          true,
		MaxOutputTokens: r.MaxTokens(),
	}
	if eff := openAIEffort(r.effort); eff != "" {
		request.Reasoning = &responsesReasoning{Effort: eff}
	}
	slog.Debug("responses request", "provider", r.provider, "model", r.model,
		"instructions_len", len(request.Instructions), "input_items", len(input), "tools", len(tools))
	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("marshal %s request: %w", r.provider, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create %s request: %w", r.provider, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if err := r.auth.Authenticate(ctx, req); err != nil {
		return nil, fmt.Errorf("authenticate: %w", err)
	}
	resp, err := openStream(r.client, req, r.provider, r.auth)
	if err != nil {
		return nil, err
	}
	return startSSEReader(ctx, resp, func(ctx context.Context, body io.ReadCloser, ch chan<- StreamEvent) {
		readResponsesSSE(ctx, body, ch, r.provider)
	}), nil
}

// --- Request types ---

type responsesRequest struct {
	Model        string              `json:"model"`
	Instructions string              `json:"instructions,omitempty"`
	Input        []any               `json:"input"`
	Tools        []responsesTool     `json:"tools,omitempty"`
	Stream       bool                `json:"stream"`
	Store        bool                `json:"store"`
	Reasoning    *responsesReasoning `json:"reasoning,omitempty"`
	// MaxOutputTokens caps the response length. Omitted (0) means the
	// provider's default applies; set after a truncated turn to give the
	// retry more headroom.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
}

// responsesReasoning carries the Responses-API reasoning control. Effort is
// one of low|medium|high.
type responsesReasoning struct {
	Effort string `json:"effort"`
}

// responsesMessageItem is a role-based message in the Responses API input.
type responsesMessageItem struct {
	Type    string `json:"type"` // "message"
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []responsesContentBlock
}

// responsesFunctionCallItem is a tool call in the Responses API input.
type responsesFunctionCallItem struct {
	Type      string `json:"type"` // "function_call"
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// responsesFunctionOutputItem is a tool result in the Responses API input.
type responsesFunctionOutputItem struct {
	Type   string `json:"type"` // "function_call_output"
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// responsesContentBlock is a typed content element in a message.
type responsesContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  FunctionParams `json:"parameters"`
}

// --- Response/SSE types ---

type responsesSSEEvent struct {
	Type     string             `json:"type"`
	Response *responsesResponse `json:"response,omitempty"`
	Item     json.RawMessage    `json:"item,omitempty"`
	Delta    string             `json:"delta,omitempty"`
	// OutputIndex keys a tool call across its added/delta/done events.
	OutputIndex int `json:"output_index"`
	// Error is the nested payload of an "error" event as the ChatGPT
	// backend sends it; see [responsesEventError] for the flat shape.
	Error *apiError `json:"error,omitempty"`
}

type responsesResponse struct {
	Usage             *responsesUsage             `json:"usage,omitempty"`
	Status            string                      `json:"status,omitempty"`
	IncompleteDetails *responsesIncompleteDetails `json:"incomplete_details,omitempty"`
	// Error explains a response.failed event.
	Error *apiError `json:"error,omitempty"`
}

// responsesIncompleteDetails is populated on a response.incomplete event when
// the model stopped before finishing. Reason is e.g. "max_output_tokens".
type responsesIncompleteDetails struct {
	Reason string `json:"reason,omitempty"`
}

type responsesUsage struct {
	InputTokens        int                          `json:"input_tokens"`
	OutputTokens       int                          `json:"output_tokens"`
	InputTokensDetails *responsesInputTokensDetails `json:"input_tokens_details,omitempty"`
}

// responsesInputTokensDetails is usage.input_tokens_details: cached_tokens
// is the part of input_tokens served from the prompt cache, reported as
// Usage.CachedTokens as the Anthropic adapter does.
type responsesInputTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type responsesOutputItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// --- Conversion: internal messages → Responses input ---

// messagesToResponsesInput converts internal messages to input items.
// Returns the system instruction (extracted from the first system message)
// and the remaining input items as typed structs.
func messagesToResponsesInput(messages []Message) (string, []any) {
	var instructions string
	var items []any

	for _, m := range messages {
		switch m.Role {
		case "system":
			// First system message becomes top-level instructions.
			if instructions == "" {
				instructions = m.Content
			} else {
				items = append(items, responsesMessageItem{
					Type:    "message",
					Role:    "developer",
					Content: m.Content,
				})
			}
		case "user":
			items = append(items, responsesMessageItem{
				Type: "message",
				Role: "user",
				Content: []responsesContentBlock{{
					Type: "input_text",
					Text: m.Content,
				}},
			})
		case "assistant":
			if m.Content != "" {
				items = append(items, responsesMessageItem{
					Type: "message",
					Role: "assistant",
					Content: []responsesContentBlock{{
						Type: "output_text",
						Text: m.Content,
					}},
				})
			}
			// Emit function_call items for tool calls.
			for _, tc := range m.ToolCalls {
				items = append(items, responsesFunctionCallItem{
					Type:      "function_call",
					CallID:    tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
		case "tool":
			items = append(items, responsesFunctionOutputItem{
				Type:   "function_call_output",
				CallID: m.ToolCallID,
				Output: m.Content,
			})
		}
	}
	return instructions, items
}

func toolsToResponsesTools(tools []ToolDef) []responsesTool {
	ct := make([]responsesTool, len(tools))
	for i, t := range tools {
		ct[i] = responsesTool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		}
	}
	return ct
}

// pendingCall accumulates a tool call from streamed deltas.
type pendingCall struct {
	id   string
	name string
	args strings.Builder
}

// responsesStreamState accumulates state across a Codex SSE stream.
type responsesStreamState struct {
	calls map[int]*pendingCall
}

// handleEvent processes a single Codex SSE event. Returns a StreamEvent to
// emit (if any; Done marks the end of the stream) or a terminal error.
func (s *responsesStreamState) handleEvent(evt responsesSSEEvent, raw []byte) (*StreamEvent, error) {
	switch evt.Type {
	case "response.output_text.delta":
		if evt.Delta != "" {
			return &StreamEvent{Token: evt.Delta}, nil
		}
	case "response.output_item.added":
		s.handleItemAdded(evt)
	case "response.function_call_arguments.delta":
		s.handleCallDelta(evt)
	case "response.output_item.done":
		s.handleItemDone(evt)
	case "response.completed", "response.incomplete":
		return s.handleCompleted(evt)
	case "error":
		return nil, responsesEventError(evt, raw).streamError()
	case "response.failed":
		var cause apiError
		if evt.Response != nil && evt.Response.Error != nil {
			cause = *evt.Response.Error
		}
		failed := cause.streamError()
		failed.what = "response failed"
		return nil, failed
	}
	return nil, nil
}

// responsesEventError extracts the payload of an "error" event. The ChatGPT
// backend nests it under "error"; the public Responses API flattens
// code/message/param onto the event itself.
func responsesEventError(evt responsesSSEEvent, raw []byte) apiError {
	if evt.Error != nil {
		return *evt.Error
	}
	var flat apiError
	if err := json.Unmarshal(raw, &flat); err != nil {
		// A field of unexpected type; whatever decoded is kept and the
		// event still ends the stream as an error.
		slog.Warn("responses: decode flat error event", "err", err)
	}
	flat.Type = "" // the event's own type ("error"), not an error class
	return flat
}

// incompleteReason reports whether a response.completed/incomplete event
// marks the response incomplete, with the provider's reason ("" if absent).
func incompleteReason(evt responsesSSEEvent) (reason string, incomplete bool) {
	incomplete = evt.Type == "response.incomplete" ||
		(evt.Response != nil && evt.Response.Status == "incomplete")
	if incomplete && evt.Response != nil && evt.Response.IncompleteDetails != nil {
		reason = evt.Response.IncompleteDetails.Reason
	}
	return reason, incomplete
}

func (s *responsesStreamState) handleItemAdded(evt responsesSSEEvent) {
	var item responsesOutputItem
	if err := json.Unmarshal(evt.Item, &item); err == nil && item.Type == "function_call" {
		s.calls[evt.OutputIndex] = &pendingCall{id: item.CallID, name: item.Name}
	}
}

func (s *responsesStreamState) handleCallDelta(evt responsesSSEEvent) {
	idx := evt.OutputIndex
	pc, ok := s.calls[idx]
	if !ok {
		return
	}
	if pc.args.Len()+len(evt.Delta) > maxToolArgBytes {
		slog.Warn("responses: tool args exceeded cap, dropping call", "output_index", idx)
		delete(s.calls, idx)
		return
	}
	pc.args.WriteString(evt.Delta)
}

func (s *responsesStreamState) handleItemDone(evt responsesSSEEvent) {
	var item responsesOutputItem
	if err := json.Unmarshal(evt.Item, &item); err == nil && item.Type == "function_call" && item.Arguments != "" {
		idx := evt.OutputIndex
		pc, ok := s.calls[idx]
		if !ok {
			return
		}
		if len(item.Arguments) > maxToolArgBytes {
			slog.Warn("responses: tool args exceeded cap, dropping call", "output_index", idx)
			delete(s.calls, idx)
			return
		}
		pc.args.Reset()
		pc.args.WriteString(item.Arguments)
	}
}

func (s *responsesStreamState) handleCompleted(evt responsesSSEEvent) (*StreamEvent, error) {
	// Only max_output_tokens has a recovery path (cap escalation through
	// Truncated). Any other stop, e.g. content_filter, is a failure: its
	// partial text and tool calls must not read as a completed turn.
	reason, incomplete := incompleteReason(evt)
	if incomplete && reason != "max_output_tokens" {
		return nil, fmt.Errorf("response incomplete: %s", cmp.Or(oneLine(reason), "no reason given"))
	}
	if incomplete {
		slog.Warn("responses: output truncated (max_output_tokens)")
	}
	return &StreamEvent{
		Done:      true,
		ToolCalls: finalizeCalls(s.calls),
		Usage:     responsesUsageOf(evt),
		Truncated: incomplete,
	}, nil
}

// responsesUsageOf converts a terminal event's usage; nil when unreported.
func responsesUsageOf(evt responsesSSEEvent) *Usage {
	if evt.Response == nil || evt.Response.Usage == nil {
		return nil
	}
	// input_tokens includes the cached ones; PromptTokens is fresh input
	// only, as in the Anthropic adapter, so consumers add Prompt + Cached
	// without asking which provider it was.
	gross := evt.Response.Usage.InputTokens
	cached := 0
	if d := evt.Response.Usage.InputTokensDetails; d != nil {
		cached = d.CachedTokens
	}
	fresh := gross - cached
	if fresh < 0 {
		// Provider mis-report (cached > gross). Clamp + drop to a
		// safe split that preserves the gross total downstream.
		slog.Warn("responses: cached_tokens > input_tokens — clamping",
			"input_tokens", gross, "cached_tokens", cached)
		fresh = gross
		cached = 0
	}
	return &Usage{
		PromptTokens:     fresh,
		CachedTokens:     cached,
		CompletionTokens: evt.Response.Usage.OutputTokens,
	}
}

// readResponsesSSE dispatches Responses SSE events through [scanSSE]:
// completed/incomplete end the stream, error/failed end it with an Err,
// EOF or [DONE] before one of them is a failure, anything else is ignored.
func readResponsesSSE(ctx context.Context, body io.ReadCloser, ch chan<- StreamEvent, provider string) {
	state := &responsesStreamState{calls: map[int]*pendingCall{}}
	scanSSE(ctx, body, ch, provider, func(_, data string) (bool, error) {
		raw := []byte(data)
		var evt responsesSSEEvent
		if err := json.Unmarshal(raw, &evt); err != nil {
			slog.Warn("responses SSE unmarshal error", "provider", provider, "err", err, "data", data[:min(len(data), 200)])
			return false, nil
		}
		emitted, err := state.handleEvent(evt, raw)
		if err != nil || emitted == nil {
			return false, err
		}
		return !trySend(ctx, ch, *emitted) || emitted.Done, nil
	}, nil)
}

// finalizeCalls converts accumulated pending calls to ToolCall slice.
// Sorts by output_index for deterministic order — does not assume contiguous indices.
func finalizeCalls(calls map[int]*pendingCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	result := make([]ToolCall, 0, len(calls))
	for _, i := range slices.Sorted(maps.Keys(calls)) {
		pc := calls[i]
		result = append(result, ToolCall{
			ID:   pc.id,
			Type: "function",
			Function: FunctionCall{
				Name:      pc.name,
				Arguments: pc.args.String(),
			},
		})
	}
	return result
}
