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
	"sync"
	"time"
)

// AgentAPI implements Provider using an OpenAI-compatible chat completions API.
type AgentAPI struct {
	auth          Auth
	baseURL       string
	model         string
	promptCaching bool
	effort        Effort // reasoning effort; "" omits reasoning_effort
	client        *http.Client

	mu        sync.Mutex
	maxTokens int // 0 means "omit from request — use provider default"
}

// NewAgentAPI creates an AgentAPI provider with the given base URL, model,
// authenticator, prompt caching flag, and reasoning effort. When promptCaching
// is true, messages are annotated with cache_control breakpoints for
// Anthropic-style prompt caching. effort "" omits the reasoning_effort field.
func NewAgentAPI(baseURL, model string, auth Auth, promptCaching bool, effort Effort) *AgentAPI {
	return &AgentAPI{
		auth:          auth,
		baseURL:       baseURL,
		model:         model,
		promptCaching: promptCaching,
		effort:        effort,
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
	Store         bool           `json:"store"`
	Tools         []ToolDef      `json:"tools,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	// MaxTokens caps the completion length. Omitted (0) means "use the
	// provider's default"; set after a truncated turn so the retry has
	// more headroom. Uses omitempty so we only start sending the field
	// once escalation has happened.
	MaxTokens int `json:"max_tokens,omitempty"`
	// ReasoningEffort selects a reasoning model's effort (low|medium|high).
	// Omitted when empty so non-reasoning models are unaffected.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// cachingChatRequest is the JSON wire format when prompt caching is enabled.
// Messages and tools use extended types that support cache_control annotations.
type cachingChatRequest struct {
	Model           string           `json:"model"`
	Messages        []cachingMessage `json:"messages"`
	Stream          bool             `json:"stream"`
	Store           bool             `json:"store"`
	Tools           []cachingToolDef `json:"tools,omitempty"`
	StreamOptions   *streamOptions   `json:"stream_options,omitempty"`
	MaxTokens       int              `json:"max_tokens,omitempty"`
	ReasoningEffort string           `json:"reasoning_effort,omitempty"`
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

// MaxTokens returns the current max_tokens value sent on requests. Zero means
// no value is sent and the provider's default applies. Safe for concurrent use.
func (a *AgentAPI) MaxTokens() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.maxTokens
}

// SetMaxTokens updates the max_tokens value used on subsequent requests.
// Used by the agent loop to escalate after a truncated response. Zero disables
// the field (falls back to the provider default). Safe for concurrent use.
func (a *AgentAPI) SetMaxTokens(v int) {
	a.mu.Lock()
	a.maxTokens = v
	a.mu.Unlock()
}

func (a *AgentAPI) Stream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamEvent, error) {
	var body []byte
	var err error

	a.mu.Lock()
	maxTokens := a.maxTokens
	a.mu.Unlock()

	effort := openAIEffort(a.effort)

	if a.promptCaching {
		cms, cts := annotateCacheBreakpoints(messages, tools)
		body, err = json.Marshal(cachingChatRequest{
			Model:           a.model,
			Messages:        cms,
			Stream:          true,
			Tools:           cts,
			StreamOptions:   includeUsage,
			MaxTokens:       maxTokens,
			ReasoningEffort: effort,
		})
	} else {
		body, err = json.Marshal(chatRequest{
			Model:           a.model,
			Messages:        messages,
			Stream:          true,
			Tools:           tools,
			StreamOptions:   includeUsage,
			MaxTokens:       maxTokens,
			ReasoningEffort: effort,
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

// merge returns false if a tool argument exceeds maxToolArgBytes or
// the index is out of bounds.
func (tc *toolCallAccumulator) merge(deltas []sseDeltaCall) bool {
	for _, d := range deltas {
		if d.Index < 0 || d.Index >= maxToolCalls {
			slog.Warn("SSE tool call index out of bounds", "index", d.Index, "max", maxToolCalls)
			return false
		}
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
			if tc.args[d.Index].Len()+len(d.Function.Arguments) > maxToolArgBytes {
				slog.Warn("SSE tool args exceeded limit", "index", d.Index, "limit", maxToolArgBytes)
				return false
			}
			tc.args[d.Index].WriteString(d.Function.Arguments)
		}
	}
	return true
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

// sseStreamState accumulates state across OpenAI-compatible SSE chunks.
type sseStreamState struct {
	tc    toolCallAccumulator
	usage *Usage
	// truncated is set when a choice reports finish_reason="length",
	// i.e. the model hit the output cap. Recorded on the finish_reason
	// chunk and surfaced on the terminal event built at [DONE]/EOF.
	truncated bool
}

// terminalEvent builds the Done event emitted from the [DONE]/EOF path in
// readSSE. The terminal event is deferred to that path (not emitted on the
// finish_reason chunk) because OpenAI's stream_options.include_usage sends
// usage in a SEPARATE trailing chunk (choices:[]) AFTER the finish_reason
// chunk; emitting Done early would drop that usage. parseUsage runs on every
// chunk, so providers that instead bundle usage into the finish_reason chunk
// still have it captured here.
func (s *sseStreamState) terminalEvent() StreamEvent {
	return StreamEvent{
		Done:      true,
		ToolCalls: s.tc.finalize(),
		Usage:     s.usage,
		Truncated: s.truncated,
	}
}

// handleChunk processes one SSE data line. Returns true to stop the stream.
func (s *sseStreamState) handleChunk(ctx context.Context, data string, ch chan<- StreamEvent) bool {
	var chunk sseChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		slog.Warn("SSE unmarshal error", "err", err, "data", data[:min(len(data), 200)])
		return false
	}

	if parsed := parseUsage(chunk.Usage); parsed != nil {
		s.usage = parsed
	}
	if len(chunk.Choices) == 0 {
		return false
	}

	choice := chunk.Choices[0]
	if delta := choice.Delta.Content; delta != "" {
		if !trySend(ctx, ch, StreamEvent{Token: delta}) {
			return true
		}
	}
	if len(choice.Delta.ToolCalls) > 0 {
		if !s.tc.merge(choice.Delta.ToolCalls) {
			// Payload-limit breach: surface a terminal error rather than
			// closing silently, so the caller sees the real cause instead
			// of the opaque errProviderClosedEarly.
			trySend(ctx, ch, StreamEvent{
				Done: true,
				Err:  fmt.Errorf("llm: tool call stream exceeded limits (max %d calls, %d arg bytes)", maxToolCalls, maxToolArgBytes),
			})
			return true
		}
	}
	if choice.FinishReason != nil {
		if *choice.FinishReason == "length" {
			s.truncated = true
			slog.Warn("openai-compat: output truncated (finish_reason=length)")
		}
		// Do NOT emit Done here: usage arrives in a separate trailing
		// chunk after this one. Keep reading; the terminal event is built
		// from the [DONE]/EOF path once that chunk (if any) is consumed.
		return false
	}
	return false
}

func (a *AgentAPI) readSSE(ctx context.Context, resp *http.Response, ch chan<- StreamEvent) {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024) // 10MB — large file content in tool results

	wd := newStreamWatchdog(resp.Body)
	defer wd.stop()

	var state sseStreamState

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
			trySend(ctx, ch, state.terminalEvent())
			return
		}
		if state.handleChunk(ctx, data, ch) {
			return
		}
	}

	// Check for scanner errors (I/O failures, buffer overflow, watchdog tear-down).
	if err := scanner.Err(); err != nil {
		if wd.fired() {
			trySend(ctx, ch, StreamEvent{Done: true, Err: errStreamStalled})
			return
		}
		if ctx.Err() == nil {
			slog.Warn("SSE scanner error", "err", err)
		}
		return // truncated stream — don't synthesize a successful Done event
	}

	// Clean EOF without [DONE]: emit the terminal event (finish_reason and/or
	// usage already captured into state).
	trySend(ctx, ch, state.terminalEvent())
}
