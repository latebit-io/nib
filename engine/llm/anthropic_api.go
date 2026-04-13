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
	"strings"
)

// AnthropicAPI implements Provider using the Anthropic Messages API.
// It translates junto's OpenAI-shaped Message/ToolCall types to the Anthropic
// content-block format and parses the Anthropic SSE streaming response.
type AnthropicAPI struct {
	auth          Auth
	baseURL       string
	model         string
	promptCaching bool
	client        *http.Client
}

// anthropicVersion is the API version header required by the Anthropic API.
const anthropicVersion = "2023-06-01"

// NewAnthropicAPI creates an AnthropicAPI provider.
func NewAnthropicAPI(baseURL, model string, auth Auth, promptCaching bool) *AnthropicAPI {
	return &AnthropicAPI{
		auth:          auth,
		baseURL:       baseURL,
		model:         model,
		promptCaching: promptCaching,
		client: &http.Client{
			Transport: agentTransport(),
			// No client-level Timeout — would kill SSE streams mid-flight.
		},
	}
}

// AnthropicKeyAuth implements Auth by setting the x-api-key header,
// which is the standard authentication method for the Anthropic API.
type AnthropicKeyAuth string

// Authenticate sets the x-api-key header with the static key.
// Skips the header if the key is empty.
func (k AnthropicKeyAuth) Authenticate(_ context.Context, req *http.Request) error {
	if k != "" {
		req.Header.Set("x-api-key", string(k))
	}
	return nil
}

// --- Anthropic request types ---

// anthropicRequest is the JSON wire format for POST /v1/messages.
type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    []anthropicContent `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicToolDef `json:"tools,omitempty"`
	Stream    bool               `json:"stream"`
	Metadata  *anthropicMetadata `json:"metadata,omitempty"`
}

// anthropicMessage is a single message in the Anthropic format.
type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []anthropicContent
}

// anthropicContent is a typed content block in the Anthropic format.
type anthropicContent struct {
	Type         string        `json:"type"`
	Text         string        `json:"text,omitempty"`
	ID           string        `json:"id,omitempty"`
	Name         string        `json:"name,omitempty"`
	Input        any           `json:"input,omitempty"`       // json.RawMessage for tool_use
	ToolUseID    string        `json:"tool_use_id,omitempty"` // tool_result only
	Content      any           `json:"content,omitempty"`     // tool_result content (string or []anthropicContent)
	IsError      bool          `json:"is_error,omitempty"`    // tool_result only
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// anthropicToolDef is the tool definition format for the Anthropic API.
type anthropicToolDef struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	InputSchema  FunctionParams `json:"input_schema"`
	CacheControl *CacheControl  `json:"cache_control,omitempty"`
}

// anthropicMetadata holds optional request metadata.
type anthropicMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

// --- Anthropic SSE response types ---

// anthropicMessageStart is the message_start event payload.
type anthropicMessageStart struct {
	Message struct {
		ID    string          `json:"id"`
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"`
}

// anthropicContentBlockStart is the content_block_start event payload.
type anthropicContentBlockStart struct {
	Index        int `json:"index"`
	ContentBlock struct {
		Type  string `json:"type"`
		ID    string `json:"id,omitempty"`
		Name  string `json:"name,omitempty"`
		Text  string `json:"text,omitempty"`
		Input string `json:"input,omitempty"`
	} `json:"content_block"`
}

// anthropicContentBlockDelta is the content_block_delta event payload.
type anthropicContentBlockDelta struct {
	Index int `json:"index"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text,omitempty"`
		PartialJSON string `json:"partial_json,omitempty"`
	} `json:"delta"`
}

// anthropicMessageDelta is the message_delta event payload.
type anthropicMessageDelta struct {
	Delta struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anthropicUsage `json:"usage"`
}

// anthropicUsage holds token consumption data from the Anthropic API.
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// --- Message translation ---

// convertAssistantMessage converts an assistant Message with optional tool calls
// into Anthropic content blocks.
func convertAssistantMessage(m Message) anthropicMessage {
	var blocks []anthropicContent
	if m.Content != "" {
		blocks = append(blocks, anthropicContent{Type: "text", Text: m.Content})
	}
	for _, tc := range m.ToolCalls {
		blocks = append(blocks, anthropicContent{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: parseToolInput(tc.Function.Arguments),
		})
	}
	return anthropicMessage{Role: "assistant", Content: blocks}
}

// parseToolInput parses a JSON arguments string into a value suitable for
// the Anthropic tool_use input field.
func parseToolInput(args string) any {
	if args == "" {
		return map[string]any{}
	}
	var input any
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return json.RawMessage(args)
	}
	return input
}

// batchToolResult appends a tool_result block to the message list, batching
// consecutive tool results into a single user message.
func batchToolResult(msgs []anthropicMessage, block anthropicContent) []anthropicMessage {
	if n := len(msgs); n > 0 {
		prev := &msgs[n-1]
		if prev.Role == "user" {
			if blocks, ok := prev.Content.([]anthropicContent); ok && len(blocks) > 0 && blocks[0].Type == "tool_result" {
				prev.Content = append(blocks, block)
				return msgs
			}
		}
	}
	return append(msgs, anthropicMessage{
		Role:    "user",
		Content: []anthropicContent{block},
	})
}

// convertToolDefs converts junto tool definitions to the Anthropic format.
func convertToolDefs(tools []ToolDef, promptCaching bool) []anthropicToolDef {
	result := make([]anthropicToolDef, len(tools))
	for i, t := range tools {
		result[i] = anthropicToolDef{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		}
	}
	if promptCaching && len(result) > 0 {
		result[len(result)-1].CacheControl = ephemeralCache
	}
	return result
}

// buildAnthropicRequest translates junto's types into the Anthropic wire format.
func (a *AnthropicAPI) buildAnthropicRequest(messages []Message, tools []ToolDef) *anthropicRequest {
	var system []anthropicContent
	var anthropicMsgs []anthropicMessage

	for i, m := range messages {
		switch m.Role {
		case "system":
			block := anthropicContent{Type: "text", Text: m.Content}
			if a.promptCaching && i == 0 {
				block.CacheControl = ephemeralCache
			}
			system = append(system, block)
		case "user":
			anthropicMsgs = append(anthropicMsgs, anthropicMessage{
				Role:    "user",
				Content: []anthropicContent{{Type: "text", Text: m.Content}},
			})
		case "assistant":
			anthropicMsgs = append(anthropicMsgs, convertAssistantMessage(m))
		case "tool":
			anthropicMsgs = batchToolResult(anthropicMsgs, anthropicContent{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   m.Content,
			})
		}
	}

	// Prompt caching breakpoint: second-to-last message.
	if a.promptCaching && len(anthropicMsgs) >= 3 {
		idx := len(anthropicMsgs) - 2
		msg := &anthropicMsgs[idx]
		if blocks, ok := msg.Content.([]anthropicContent); ok && len(blocks) > 0 {
			blocks[len(blocks)-1].CacheControl = ephemeralCache
			msg.Content = blocks
		}
	}

	return &anthropicRequest{
		Model:     a.model,
		MaxTokens: 16384,
		System:    system,
		Messages:  anthropicMsgs,
		Tools:     convertToolDefs(tools, a.promptCaching),
		Stream:    true,
	}
}

// --- Streaming ---

// Stream sends a chat completion request to the Anthropic Messages API
// and returns a channel of streaming events.
func (a *AnthropicAPI) Stream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamEvent, error) {
	apiReq := a.buildAnthropicRequest(messages, tools)
	body, err := json.Marshal(apiReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	base := strings.TrimRight(a.baseURL, "/")
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", anthropicVersion)
	if err := a.auth.Authenticate(ctx, req); err != nil {
		return nil, fmt.Errorf("authenticate: %w", err)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("api error: status %d: %s", resp.StatusCode, string(respBody))
	}

	ch := make(chan StreamEvent, 16)
	go func() {
		defer func() { _ = resp.Body.Close() }()
		defer close(ch)
		a.readAnthropicSSE(ctx, resp, ch)
	}()

	return ch, nil
}

// anthropicBlockState tracks the state of an in-progress content block.
type anthropicBlockState struct {
	blockType string // "text" or "tool_use"
	toolID    string
	toolName  string
	args      strings.Builder
}

// anthropicStreamState accumulates state across the SSE stream.
type anthropicStreamState struct {
	usage      *Usage
	blocks     map[int]*anthropicBlockState
	inputUsage *anthropicUsage
}

// handleBlockStart processes a content_block_start event.
func (s *anthropicStreamState) handleBlockStart(data string) {
	var evt anthropicContentBlockStart
	if err := json.Unmarshal([]byte(data), &evt); err != nil {
		slog.Warn("anthropic: unmarshal content_block_start", "err", err, "data", data[:min(len(data), 200)])
		return
	}
	s.blocks[evt.Index] = &anthropicBlockState{
		blockType: evt.ContentBlock.Type,
		toolID:    evt.ContentBlock.ID,
		toolName:  evt.ContentBlock.Name,
	}
}

// handleBlockDelta processes a content_block_delta event.
// Returns a non-empty token string for text deltas.
func (s *anthropicStreamState) handleBlockDelta(data string) string {
	var evt anthropicContentBlockDelta
	if err := json.Unmarshal([]byte(data), &evt); err != nil {
		slog.Warn("anthropic: unmarshal content_block_delta", "err", err, "data", data[:min(len(data), 200)])
		return ""
	}
	block, ok := s.blocks[evt.Index]
	if !ok {
		slog.Warn("anthropic: delta for unknown block", "index", evt.Index)
		return ""
	}
	switch evt.Delta.Type {
	case "text_delta":
		return evt.Delta.Text
	case "input_json_delta":
		block.args.WriteString(evt.Delta.PartialJSON)
	}
	return ""
}

// handleMessageDelta processes a message_delta event.
func (s *anthropicStreamState) handleMessageDelta(data string) {
	var evt anthropicMessageDelta
	if err := json.Unmarshal([]byte(data), &evt); err != nil {
		slog.Warn("anthropic: unmarshal message_delta", "err", err, "data", data[:min(len(data), 200)])
		return
	}
	s.usage = mergeAnthropicUsage(s.inputUsage, evt.Usage)
}

// finalEvent builds the terminal StreamEvent from accumulated state.
func (s *anthropicStreamState) finalEvent() StreamEvent {
	return StreamEvent{Done: true, ToolCalls: finalizeAnthropicBlocks(s.blocks), Usage: s.usage}
}

// anthropicEventResult signals the outcome of processing one SSE event.
type anthropicEventResult int

const (
	anthropicContinue anthropicEventResult = iota
	anthropicDone
)

// dispatchEvent processes a single SSE event, returning whether the stream
// should continue or is done. Sends text tokens to ch as they arrive.
func (s *anthropicStreamState) dispatchEvent(eventType, data string, ch chan<- StreamEvent) anthropicEventResult {
	switch eventType {
	case "message_start":
		var evt anthropicMessageStart
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			slog.Warn("anthropic: unmarshal message_start", "err", err, "data", data[:min(len(data), 200)])
			return anthropicContinue
		}
		s.inputUsage = evt.Message.Usage
	case "content_block_start":
		s.handleBlockStart(data)
	case "content_block_delta":
		if token := s.handleBlockDelta(data); token != "" {
			ch <- StreamEvent{Token: token}
		}
	case "message_delta":
		s.handleMessageDelta(data)
	case "message_stop":
		ch <- s.finalEvent()
		return anthropicDone
	case "error":
		slog.Warn("anthropic: stream error event", "data", data[:min(len(data), 500)])
		ch <- s.finalEvent()
		return anthropicDone
	}
	return anthropicContinue
}

// readAnthropicSSE parses the Anthropic SSE stream and sends StreamEvents.
func (a *AnthropicAPI) readAnthropicSSE(ctx context.Context, resp *http.Response, ch chan<- StreamEvent) {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	state := &anthropicStreamState{blocks: make(map[int]*anthropicBlockState)}
	var eventType string

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line := scanner.Text()

		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))

		if state.dispatchEvent(eventType, data, ch) == anthropicDone {
			return
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		slog.Warn("anthropic: SSE scanner error", "err", err)
	}
	if ctx.Err() == nil {
		ch <- state.finalEvent()
	}
}

// mergeAnthropicUsage combines input usage (from message_start) with output
// usage (from message_delta) into the junto Usage type.
func mergeAnthropicUsage(input, output *anthropicUsage) *Usage {
	u := &Usage{}
	if input != nil {
		u.PromptTokens = input.InputTokens
		u.CachedTokens = input.CacheReadInputTokens
	}
	if output != nil {
		u.CompletionTokens = output.OutputTokens
		// Output usage also reports cache tokens — prefer the latest.
		if output.CacheReadInputTokens > 0 {
			u.CachedTokens = output.CacheReadInputTokens
		}
	}
	return u
}

// finalizeAnthropicBlocks extracts completed tool calls from the block state.
func finalizeAnthropicBlocks(blocks map[int]*anthropicBlockState) []ToolCall {
	var calls []ToolCall
	// Iterate in index order for deterministic output.
	for i := 0; i < len(blocks); i++ {
		block, ok := blocks[i]
		if !ok {
			continue
		}
		if block.blockType != "tool_use" {
			continue
		}
		calls = append(calls, ToolCall{
			ID:   block.toolID,
			Type: "function",
			Function: FunctionCall{
				Name:      block.toolName,
				Arguments: block.args.String(),
			},
		})
	}
	return calls
}

// anthropicModelsResponse is the JSON response from GET /v1/models.
type anthropicModelsResponse struct {
	Data []struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"data"`
}

// ListModels queries the Anthropic /v1/models endpoint.
// This validates auth and returns available models.
func (a *AnthropicAPI) ListModels(ctx context.Context) ([]ModelInfo, error) {
	base := strings.TrimRight(a.baseURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("llm: create request: %w", err)
	}
	req.Header.Set("anthropic-version", anthropicVersion)
	if err := a.auth.Authenticate(ctx, req); err != nil {
		return nil, fmt.Errorf("llm: authenticate: %w", err)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: list models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // HTTP response body close rarely fails

	if resp.StatusCode != http.StatusOK {
		_, _ = io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("llm: list models: HTTP %d", resp.StatusCode)
	}

	var result anthropicModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("llm: decode models response: %w", err)
	}

	models := make([]ModelInfo, len(result.Data))
	for i, m := range result.Data {
		name := m.DisplayName
		if name == "" {
			name = m.ID
		}
		models[i] = ModelInfo{ID: m.ID, Name: name}
	}
	return models, nil
}
