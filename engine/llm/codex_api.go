package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
)

// CodexAPI implements Provider using the OpenAI Responses API format,
// used by the ChatGPT/Codex subscription endpoint.
type CodexAPI struct {
	auth   Auth
	model  string
	client *http.Client
}

// NewCodexAPI creates a CodexAPI provider for the ChatGPT Codex endpoint.
func NewCodexAPI(model string, auth Auth) *CodexAPI {
	return &CodexAPI{
		auth:  auth,
		model: model,
		client: &http.Client{
			Transport: agentTransport(),
		},
	}
}

// Codex API endpoints.
const (
	codexEndpoint       = "https://chatgpt.com/backend-api/codex/responses"
	codexModelsEndpoint = "https://chatgpt.com/backend-api/codex/models?client_version=0.1.0"
)

// CodexModelInfo describes an available model from the Codex API.
type CodexModelInfo struct {
	// Slug is the model identifier used in API requests.
	Slug string
	// DisplayName is the human-readable name.
	DisplayName string
}

// ListModels queries the Codex models endpoint for available models.
// Implements ModelLister.
func (c *CodexAPI) ListModels(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexModelsEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("codex: create models request: %w", err)
	}
	if err := c.auth.Authenticate(ctx, req); err != nil {
		return nil, fmt.Errorf("codex: authenticate: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex: list models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // body already read; close error is not actionable

	if resp.StatusCode != http.StatusOK {
		// Body drained by deferred Close; no need to read it.
		return nil, fmt.Errorf("codex: list models: HTTP %d", resp.StatusCode)
	}

	var result struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("codex: decode models: %w", err)
	}

	models := make([]ModelInfo, len(result.Models))
	for i, m := range result.Models {
		name := m.DisplayName
		if name == "" {
			name = m.Slug
		}
		models[i] = ModelInfo{ID: m.Slug, Name: name}
	}
	return models, nil
}

// --- Request types (Responses API wire format) ---

type codexRequest struct {
	Model        string      `json:"model"`
	Instructions string      `json:"instructions"`
	Input        []any       `json:"input"`
	Tools        []codexTool `json:"tools,omitempty"`
	Stream       bool        `json:"stream"`
	Store        bool        `json:"store"`
}

// codexMessageItem is a role-based message in the Responses API input.
type codexMessageItem struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []codexContentBlock
}

// codexFunctionCallItem is a tool call in the Responses API input.
type codexFunctionCallItem struct {
	Type      string `json:"type"` // "function_call"
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// codexFunctionOutputItem is a tool result in the Responses API input.
type codexFunctionOutputItem struct {
	Type   string `json:"type"` // "function_call_output"
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// codexContentBlock is a typed content element in a message.
type codexContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type codexTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  FunctionParams `json:"parameters"`
	Strict      bool           `json:"strict,omitempty"`
}

// --- Response/SSE types ---

type codexSSEEvent struct {
	Type     string          `json:"type"`
	Response *codexResponse  `json:"response,omitempty"`
	Item     json.RawMessage `json:"item,omitempty"`
	Delta    string          `json:"delta,omitempty"`
	ItemID   string          `json:"item_id,omitempty"`
}

type codexResponse struct {
	Usage *codexUsage `json:"usage,omitempty"`
}

type codexUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type codexOutputItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Status    string `json:"status,omitempty"`
}

// --- Conversion: Junto messages → Codex input ---

// messagesToCodexInput converts Junto messages to Codex input items.
// Returns the system instruction (extracted from the first system message)
// and the remaining input items as typed structs.
func messagesToCodexInput(messages []Message) (string, []any) {
	var instructions string
	var items []any

	for _, m := range messages {
		switch m.Role {
		case "system":
			// First system message becomes top-level instructions.
			if instructions == "" {
				instructions = m.Content
			} else {
				items = append(items, codexMessageItem{
					Role:    "developer",
					Content: m.Content,
				})
			}
		case "user":
			items = append(items, codexMessageItem{
				Role: "user",
				Content: []codexContentBlock{{
					Type: "input_text",
					Text: m.Content,
				}},
			})
		case "assistant":
			if m.Content != "" {
				items = append(items, codexMessageItem{
					Role: "assistant",
					Content: []codexContentBlock{{
						Type: "output_text",
						Text: m.Content,
					}},
				})
			}
			// Emit function_call items for tool calls.
			for _, tc := range m.ToolCalls {
				items = append(items, codexFunctionCallItem{
					Type:      "function_call",
					CallID:    tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
		case "tool":
			items = append(items, codexFunctionOutputItem{
				Type:   "function_call_output",
				CallID: m.ToolCallID,
				Output: m.Content,
			})
		}
	}
	return instructions, items
}

func toolsToCodexTools(tools []ToolDef) []codexTool {
	ct := make([]codexTool, len(tools))
	for i, t := range tools {
		ct[i] = codexTool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		}
	}
	return ct
}

// --- Provider implementation ---

// Stream sends a Responses API request to the Codex endpoint and returns
// streaming events compatible with Junto's StreamEvent type.
func (c *CodexAPI) Stream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamEvent, error) {
	instructions, input := messagesToCodexInput(messages)
	if instructions == "" {
		instructions = "You are a helpful coding assistant."
	}
	slog.Debug("codex request", "model", c.model, "instructions_len", len(instructions), "input_items", len(input), "tools", len(tools))
	reqBody := codexRequest{
		Model:        c.model,
		Instructions: instructions,
		Input:        input,
		Tools:        toolsToCodexTools(tools),
		Stream:       true,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal codex request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", codexEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create codex request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.auth.Authenticate(ctx, req); err != nil {
		return nil, fmt.Errorf("authenticate: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 2048))
		_ = resp.Body.Close() // body drained above; close error is not actionable
		if readErr != nil {
			return nil, fmt.Errorf("codex error: status %d (body unreadable: %w)", resp.StatusCode, readErr)
		}
		return nil, fmt.Errorf("codex error: status %d: %s", resp.StatusCode, string(respBody))
	}

	ch := make(chan StreamEvent, 16)
	go func() {
		defer func() { _ = resp.Body.Close() }() // SSE stream done; close error is not actionable
		defer close(ch)
		c.readCodexSSE(ctx, resp, ch)
	}()

	return ch, nil
}

// pendingCall accumulates a tool call from streamed deltas.
type pendingCall struct {
	id   string
	name string
	args strings.Builder
}

// codexStreamState accumulates state across a Codex SSE stream.
type codexStreamState struct {
	calls map[int]*pendingCall
	usage *Usage
}

// handleEvent processes a single Codex SSE event. Returns a StreamEvent
// to emit (if any) and whether the stream is complete.
func (s *codexStreamState) handleEvent(evt codexSSEEvent, raw []byte) (emitted *StreamEvent, done bool) {
	switch evt.Type {
	case "response.output_text.delta":
		if evt.Delta != "" {
			return &StreamEvent{Token: evt.Delta}, false
		}
	case "response.output_item.added":
		s.handleItemAdded(evt, raw)
	case "response.function_call_arguments.delta":
		s.handleCallDelta(evt, raw)
	case "response.output_item.done":
		s.handleItemDone(evt, raw)
	case "response.completed", "response.incomplete":
		return s.handleCompleted(evt)
	}
	return nil, false
}

func (s *codexStreamState) handleItemAdded(evt codexSSEEvent, raw []byte) {
	var item codexOutputItem
	if err := json.Unmarshal(evt.Item, &item); err == nil && item.Type == "function_call" {
		idx := extractOutputIndex(raw)
		s.calls[idx] = &pendingCall{id: item.CallID, name: item.Name}
	}
}

// maxToolArgsBytes caps accumulated tool-call arguments to prevent unbounded
// growth from streamed model output. Matches the SSE scanner's 10MB limit.
const maxToolArgsBytes = 10 * 1024 * 1024

func (s *codexStreamState) handleCallDelta(evt codexSSEEvent, raw []byte) {
	idx := extractOutputIndex(raw)
	pc, ok := s.calls[idx]
	if !ok {
		return
	}
	if pc.args.Len()+len(evt.Delta) > maxToolArgsBytes {
		slog.Warn("codex: tool args exceeded cap, dropping call", "output_index", idx)
		delete(s.calls, idx)
		return
	}
	pc.args.WriteString(evt.Delta)
}

func (s *codexStreamState) handleItemDone(evt codexSSEEvent, raw []byte) {
	var item codexOutputItem
	if err := json.Unmarshal(evt.Item, &item); err == nil && item.Type == "function_call" && item.Arguments != "" {
		idx := extractOutputIndex(raw)
		pc, ok := s.calls[idx]
		if !ok {
			return
		}
		if len(item.Arguments) > maxToolArgsBytes {
			slog.Warn("codex: tool args exceeded cap, dropping call", "output_index", idx)
			delete(s.calls, idx)
			return
		}
		pc.args.Reset()
		pc.args.WriteString(item.Arguments)
	}
}

func (s *codexStreamState) handleCompleted(evt codexSSEEvent) (*StreamEvent, bool) {
	if evt.Response != nil && evt.Response.Usage != nil {
		s.usage = &Usage{
			PromptTokens:     evt.Response.Usage.InputTokens,
			CompletionTokens: evt.Response.Usage.OutputTokens,
		}
	}
	final := StreamEvent{Done: true, ToolCalls: finalizeCalls(s.calls), Usage: s.usage}
	return &final, true
}

// extractOutputIndex parses the output_index field from raw SSE JSON.
func extractOutputIndex(raw []byte) int {
	var v struct {
		OutputIndex int `json:"output_index"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		slog.Debug("codex: extractOutputIndex unmarshal failed", "err", err)
	}
	return v.OutputIndex
}

// readCodexSSE parses the Responses API SSE stream into StreamEvents.
func (c *CodexAPI) readCodexSSE(ctx context.Context, resp *http.Response, ch chan<- StreamEvent) {
	// send writes an event to the channel or returns false if ctx is cancelled.
	// Prevents blocking indefinitely if the consumer stops reading.
	send := func(ev StreamEvent) bool {
		select {
		case ch <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	state := &codexStreamState{calls: map[int]*pendingCall{}}

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			send(StreamEvent{Done: true, ToolCalls: finalizeCalls(state.calls), Usage: state.usage})
			return
		}

		raw := []byte(data)
		var evt codexSSEEvent
		if err := json.Unmarshal(raw, &evt); err != nil {
			slog.Warn("codex SSE unmarshal error", "err", err, "data", data[:min(len(data), 200)])
			continue
		}

		if emitted, done := state.handleEvent(evt, raw); emitted != nil {
			if !send(*emitted) {
				return
			}
			if done {
				return
			}
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		slog.Warn("codex SSE scanner error", "err", err)
	}
	if ctx.Err() == nil {
		send(StreamEvent{Done: true, ToolCalls: finalizeCalls(state.calls), Usage: state.usage})
	}
}

// finalizeCalls converts accumulated pending calls to ToolCall slice.
// Sorts by output_index for deterministic order — does not assume contiguous indices.
func finalizeCalls(calls map[int]*pendingCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	indices := make([]int, 0, len(calls))
	for i := range calls {
		indices = append(indices, i)
	}
	slices.Sort(indices)

	result := make([]ToolCall, 0, len(indices))
	for _, i := range indices {
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
