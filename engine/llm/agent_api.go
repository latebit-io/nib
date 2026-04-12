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
	"time"
)

// AgentAPI implements Provider using an OpenAI-compatible chat completions API.
type AgentAPI struct {
	auth          Auth
	baseURL       string
	model         string
	promptCaching bool
	client        *http.Client
}

// NewAgentAPI creates an AgentAPI provider with the given base URL, model,
// authenticator, and prompt caching flag. When promptCaching is true, messages
// are annotated with cache_control breakpoints for Anthropic-style prompt caching.
func NewAgentAPI(baseURL, model string, auth Auth, promptCaching bool) *AgentAPI {
	return &AgentAPI{
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

// agentTransport clones http.DefaultTransport and adds connection timeouts.
// Preserves proxy support, keep-alive, and other defaults where possible.
func agentTransport() *http.Transport {
	if dt, ok := http.DefaultTransport.(*http.Transport); ok && dt != nil {
		t := dt.Clone()
		t.ResponseHeaderTimeout = 30 * time.Second
		return t
	}
	return &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
	}
}

// streamOptions requests usage data in streaming responses.
type streamOptions struct {
	// IncludeUsage asks the provider to include token counts on the final chunk.
	IncludeUsage bool `json:"include_usage"`
}

// includeUsage is the shared stream_options value — always request usage data.
var includeUsage = &streamOptions{IncludeUsage: true}

type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []Message      `json:"messages"`
	Stream        bool           `json:"stream"`
	Tools         []ToolDef      `json:"tools,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

// cachingChatRequest is the JSON wire format when prompt caching is enabled.
// Messages and tools use extended types that support cache_control annotations.
type cachingChatRequest struct {
	Model         string           `json:"model"`
	Messages      []cachingMessage `json:"messages"`
	Stream        bool             `json:"stream"`
	Tools         []cachingToolDef `json:"tools,omitempty"`
	StreamOptions *streamOptions   `json:"stream_options,omitempty"`
}

// cachingMessage extends Message with support for content blocks.
// When CacheControl is set, Content is serialized as []contentBlock
// instead of a plain string so the cache_control field can be attached.
type cachingMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"` // string or []contentBlock; nil omits the field
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// contentBlock is a typed content element with optional cache control.
type contentBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// cachingToolDef extends ToolDef with optional cache control.
type cachingToolDef struct {
	Type         string        `json:"type"`
	Function     FunctionDef   `json:"function"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// ephemeralCache is the standard Anthropic cache control value.
var ephemeralCache = &CacheControl{Type: "ephemeral"}

// annotateCacheBreakpoints transforms messages and tools for Anthropic-style
// prompt caching. Places cache_control breakpoints on:
//  1. The system message (static across turns)
//  2. The last tool definition (static within a mode)
//  3. The second-to-last message (caches the growing conversation prefix)
func annotateCacheBreakpoints(messages []Message, tools []ToolDef) ([]cachingMessage, []cachingToolDef) {
	cms := make([]cachingMessage, len(messages))
	for i, m := range messages {
		// Convert empty content to nil so omitempty drops the field,
		// matching Message's json:"content,omitempty" behavior.
		var content any
		if m.Content != "" {
			content = m.Content
		}
		cms[i] = cachingMessage{
			Role:       m.Role,
			Content:    content,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
		}
	}

	// Breakpoint 1: system message → content blocks with cache_control.
	if len(cms) > 0 && cms[0].Role == "system" {
		if s, ok := cms[0].Content.(string); ok && s != "" {
			cms[0].Content = []contentBlock{{
				Type:         "text",
				Text:         s,
				CacheControl: ephemeralCache,
			}}
		}
	}

	// Breakpoint 3: second-to-last message (caches conversation prefix).
	// The last message is the new content; everything before it is stable.
	if len(cms) >= 3 {
		idx := len(cms) - 2
		if s, ok := cms[idx].Content.(string); ok && s != "" {
			cms[idx].Content = []contentBlock{{
				Type:         "text",
				Text:         s,
				CacheControl: ephemeralCache,
			}}
		}
	}

	// Breakpoint 2: last tool definition.
	cts := make([]cachingToolDef, len(tools))
	for i, t := range tools {
		cts[i] = cachingToolDef{
			Type:     t.Type,
			Function: t.Function,
		}
	}
	if len(cts) > 0 {
		cts[len(cts)-1].CacheControl = ephemeralCache
	}

	return cms, cts
}

type sseChunk struct {
	Choices []struct {
		Delta struct {
			Content   string         `json:"content"`
			ToolCalls []sseDeltaCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *sseUsage `json:"usage,omitempty"`
}

// sseUsage is the token consumption data from the provider.
type sseUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	PromptDetails    *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

// sseDeltaCall is a partial tool call from an SSE delta.
// On the first chunk for a tool call, ID and Function.Name are set.
// On subsequent chunks, only Function.Arguments is appended.
type sseDeltaCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

// Stream sends a chat completion request and returns a channel of streaming events.
func (a *AgentAPI) Stream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamEvent, error) {
	var body []byte
	var err error

	if a.promptCaching {
		cms, cts := annotateCacheBreakpoints(messages, tools)
		body, err = json.Marshal(cachingChatRequest{
			Model:         a.model,
			Messages:      cms,
			Stream:        true,
			Tools:         cts,
			StreamOptions: includeUsage,
		})
	} else {
		body, err = json.Marshal(chatRequest{
			Model:         a.model,
			Messages:      messages,
			Stream:        true,
			Tools:         tools,
			StreamOptions: includeUsage,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	base := strings.TrimRight(a.baseURL, "/")
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
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
		a.readSSE(ctx, resp, ch)
	}()

	return ch, nil
}

// toolCallAccumulator accumulates streamed tool call deltas.
// Uses []*strings.Builder (not []strings.Builder) because strings.Builder
// must not be copied after first use, and slice append can reallocate.
type toolCallAccumulator struct {
	calls []ToolCall
	args  []*strings.Builder
}

func (tc *toolCallAccumulator) merge(deltas []sseDeltaCall) {
	for _, d := range deltas {
		// Grow slices if needed
		for d.Index >= len(tc.calls) {
			tc.calls = append(tc.calls, ToolCall{})
			tc.args = append(tc.args, &strings.Builder{})
		}
		if d.ID != "" {
			tc.calls[d.Index].ID = d.ID
		}
		if d.Type != "" {
			tc.calls[d.Index].Type = d.Type
		}
		if d.Function.Name != "" {
			tc.calls[d.Index].Function.Name = d.Function.Name
		}
		if d.Function.Arguments != "" {
			tc.args[d.Index].WriteString(d.Function.Arguments)
		}
	}
}

func (tc *toolCallAccumulator) finalize() []ToolCall {
	for i := range tc.calls {
		tc.calls[i].Function.Arguments = tc.args[i].String()
	}
	return tc.calls
}

// parseUsage converts provider-reported usage into our Usage type.
// Returns nil when the provider didn't report usage.
func parseUsage(u *sseUsage) *Usage {
	if u == nil {
		return nil
	}
	usage := &Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
	}
	if u.PromptDetails != nil {
		usage.CachedTokens = u.PromptDetails.CachedTokens
	}
	return usage
}

func (a *AgentAPI) readSSE(ctx context.Context, resp *http.Response, ch chan<- StreamEvent) {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024) // 10MB — large file content in tool results

	var tc toolCallAccumulator
	var usage *Usage // accumulated across chunks — may arrive before or with finish_reason

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
			ch <- StreamEvent{Done: true, ToolCalls: tc.finalize(), Usage: usage}
			return
		}

		var chunk sseChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			slog.Warn("SSE unmarshal error", "err", err, "data", data[:min(len(data), 200)])
			continue
		}

		// Usage may appear on any chunk (often the last or a trailing chunk).
		if parsed := parseUsage(chunk.Usage); parsed != nil {
			usage = parsed
		}

		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]

		// Text content
		if delta := choice.Delta.Content; delta != "" {
			ch <- StreamEvent{Token: delta}
		}

		// Tool call deltas
		if len(choice.Delta.ToolCalls) > 0 {
			tc.merge(choice.Delta.ToolCalls)
		}

		if choice.FinishReason != nil {
			ch <- StreamEvent{Done: true, ToolCalls: tc.finalize(), Usage: usage}
			return
		}
	}

	// Check for scanner errors (I/O failures, buffer overflow)
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		slog.Warn("SSE scanner error", "err", err)
	}

	// Stream ended without [DONE] or finish_reason (EOF or scanner error).
	if ctx.Err() == nil {
		ch <- StreamEvent{Done: true, ToolCalls: tc.finalize(), Usage: usage}
	}
}
