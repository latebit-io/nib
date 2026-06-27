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
	"sync"
)

// CodexAPI implements Provider using the OpenAI Responses API format,
// used by the ChatGPT/Codex subscription endpoint.
type CodexAPI struct {
	auth   Auth
	model  string
	client *http.Client

	mu        sync.Mutex
	maxTokens int // 0 means "omit max_output_tokens — use provider default"
}

// authFailure handles a 401 from the Codex endpoint, shared by Stream and
// ListModels so both self-heal identically. The token source already
// refreshed during Authenticate, so a 401 here means a dead/revoked token:
// discard it via the optional invalidation port (a no-op for static
// API-key auth) so the next launch detects "no credential" and re-offers
// connect, then return a typed, actionable [AuthError] instead of the raw
// body.
func (c *CodexAPI) authFailure(statusCode int, body []byte) error {
	if inv, ok := c.auth.(CredentialInvalidator); ok {
		inv.Invalidate()
	}
	return parseAuthError("chatgpt", statusCode, body)
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
		// A 401 here is a revoked/dead token just as in Stream — apply
		// the same self-heal so a token first rejected during model
		// listing (e.g. at startup) is cleared and surfaced as a typed
		// AuthError. Other statuses keep the generic message.
		if resp.StatusCode == http.StatusUnauthorized {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2048))
			if readErr != nil {
				// The body only enriches the AuthError's Code/Message;
				// parseAuthError tolerates an empty body and the 401 is
				// authoritative, so an unreadable body still self-heals.
				body = nil
			}
			return nil, c.authFailure(resp.StatusCode, body)
		}
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
	// MaxOutputTokens caps the response length. Omitted (0) means the
	// provider's default applies; set after a truncated turn to give the
	// retry more headroom.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
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
	Usage             *codexUsage             `json:"usage,omitempty"`
	Status            string                  `json:"status,omitempty"`
	IncompleteDetails *codexIncompleteDetails `json:"incomplete_details,omitempty"`
}

// codexIncompleteDetails is populated on a response.incomplete event when
// the model stopped before finishing. Reason is e.g. "max_output_tokens".
type codexIncompleteDetails struct {
	Reason string `json:"reason,omitempty"`
}

type codexUsage struct {
	InputTokens        int                      `json:"input_tokens"`
	OutputTokens       int                      `json:"output_tokens"`
	InputTokensDetails *codexInputTokensDetails `json:"input_tokens_details,omitempty"`
}

// codexInputTokensDetails carries the cached-prefix breakdown the
// Responses API reports under usage.input_tokens_details. cached_tokens
// is the slice of input_tokens served from OpenAI's automatic prompt
// cache — billed at a discount and surfaced through Usage.CachedTokens
// so the TUI's cache-hit indicator can light up for Codex sessions the
// same way it does for Anthropic.
type codexInputTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type codexOutputItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Status    string `json:"status,omitempty"`
}

// --- Conversion: internal messages → Codex input ---

// messagesToCodexInput converts internal messages to Codex input items.
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

// MaxTokens returns the current max_output_tokens value sent on requests.
// Zero means no value is sent and the provider's default applies.
func (c *CodexAPI) MaxTokens() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxTokens
}

// SetMaxTokens updates the max_output_tokens value used on subsequent
// requests. Used by the agent loop to escalate after a truncated response.
// Zero falls back to the provider default.
func (c *CodexAPI) SetMaxTokens(v int) {
	c.mu.Lock()
	c.maxTokens = v
	c.mu.Unlock()
}

func (c *CodexAPI) Stream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamEvent, error) {
	instructions, input := messagesToCodexInput(messages)
	if instructions == "" {
		instructions = "You are a helpful coding assistant."
	}
	slog.Debug("codex request", "model", c.model, "instructions_len", len(instructions), "input_items", len(input), "tools", len(tools))
	c.mu.Lock()
	maxTokens := c.maxTokens
	c.mu.Unlock()

	reqBody := codexRequest{
		Model:           c.model,
		Instructions:    instructions,
		Input:           input,
		Tools:           toolsToCodexTools(tools),
		Stream:          true,
		MaxOutputTokens: maxTokens,
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
		// Check 401 before the read-error guard: the body only enriches
		// the AuthError's Code/Message, but the 401 itself is
		// authoritative, so an unreadable body must still self-heal
		// (invalidate the credential) rather than fall through to a
		// generic error that leaves the dead token in the store.
		if resp.StatusCode == http.StatusUnauthorized {
			if readErr != nil {
				respBody = nil
			}
			return nil, c.authFailure(resp.StatusCode, respBody)
		}
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

// isTruncatedCompletion reports whether a response.completed/response.incomplete
// event indicates the model specifically hit the output token cap. Requires
// positive confirmation via incomplete_details.reason == "max_output_tokens";
// other incomplete reasons (e.g. "content_filter") are not token truncation
// and must not trigger max_tokens escalation — bumping the cap would not help
// a content-filter refusal and would waste tokens on retries.
//
// Anomalous incomplete events (no incomplete_details, or a non-truncation
// reason) are logged so we retain observability without silently discarding
// the signal.
func isTruncatedCompletion(evt codexSSEEvent) bool {
	incomplete := evt.Type == "response.incomplete" ||
		(evt.Response != nil && evt.Response.Status == "incomplete")
	if !incomplete {
		return false
	}
	if evt.Response == nil || evt.Response.IncompleteDetails == nil {
		slog.Warn("codex: incomplete response without incomplete_details — cannot classify",
			"type", evt.Type)
		return false
	}
	reason := evt.Response.IncompleteDetails.Reason
	if reason == "max_output_tokens" {
		return true
	}
	slog.Warn("codex: incomplete response with non-truncation reason — not escalating",
		"reason", reason)
	return false
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
		// Normalize so PromptTokens means "fresh uncached input" — the
		// Responses API returns input_tokens as the GROSS total
		// (including cached); subtract the cached subset so the field
		// has the same semantics as the Anthropic adapter's
		// PromptTokens. Downstream consumers (TUI display, kit/budget,
		// billing logs) then compute total_input = Prompt + Cached
		// without provider branching. Matches opencode's
		// `adjustedInputTokens` (session.ts::getUsage).
		gross := evt.Response.Usage.InputTokens
		cached := 0
		if d := evt.Response.Usage.InputTokensDetails; d != nil {
			cached = d.CachedTokens
		}
		fresh := gross - cached
		if fresh < 0 {
			// Provider mis-report (cached > gross). Clamp + drop to a
			// safe split that preserves the gross total downstream.
			slog.Warn("codex: cached_tokens > input_tokens — clamping",
				"input_tokens", gross, "cached_tokens", cached)
			fresh = gross
			cached = 0
		}
		s.usage = &Usage{
			PromptTokens:     fresh,
			CachedTokens:     cached,
			CompletionTokens: evt.Response.Usage.OutputTokens,
		}
	}
	truncated := isTruncatedCompletion(evt)
	if truncated {
		slog.Warn("codex: output truncated (response.incomplete / max_output_tokens)")
	}
	final := StreamEvent{
		Done:      true,
		ToolCalls: finalizeCalls(s.calls),
		Usage:     s.usage,
		Truncated: truncated,
	}
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

	wd := newStreamWatchdog(resp.Body)
	defer wd.stop()

	state := &codexStreamState{calls: map[int]*pendingCall{}}

	for scanner.Scan() {
		wd.reset()
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

	if err := scanner.Err(); err != nil {
		if wd.fired() {
			send(StreamEvent{Done: true, Err: errStreamStalled})
			return
		}
		if ctx.Err() == nil {
			slog.Warn("codex SSE scanner error", "err", err)
		}
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
