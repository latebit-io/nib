package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Message represents a single message in a conversation.
type Message struct {
	// Role is the message role: "system", "user", "assistant", or "tool".
	Role string `json:"role"`
	// Content is the text content of the message.
	Content string `json:"content,omitempty"`
	// ToolCalls are the tool calls requested by the assistant (assistant role only).
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID identifies which tool call this result is for (tool role only).
	ToolCallID string `json:"tool_call_id,omitempty"`
	// Reasoning is the provider's reasoning trace for an assistant turn
	// (extended thinking), preserved so it can be replayed on later
	// requests. Nil for turns without one and for providers that do not
	// produce reasoning. It rides the message through history — some
	// providers (Anthropic extended thinking) REQUIRE the trace, with its
	// signature, replayed verbatim on tool-use turns or the request fails.
	// Structurally parallel to [Message.ToolCalls]: provider-produced
	// content that belongs to the assistant turn.
	Reasoning *ReasoningTrace `json:"reasoning,omitempty"`
}

// ReasoningTrace is an assistant turn's preserved reasoning, as an ordered
// list of blocks. Provider-agnostic so an OpenAI-style reasoning-item trace
// can reuse it; today only the Anthropic adapter populates it.
type ReasoningTrace struct {
	// Blocks are the reasoning blocks in the order the model produced them,
	// which must be preserved on replay.
	Blocks []ReasoningBlock
}

// ReasoningBlock is one block of a [ReasoningTrace].
type ReasoningBlock struct {
	// Type is the block kind: "thinking" (Text + Signature) or
	// "redacted_thinking" (opaque Data, no readable text).
	Type string
	// Text is the visible reasoning text for a "thinking" block.
	Text string
	// Signature is the provider's cryptographic signature over a "thinking"
	// block, required verbatim on replay.
	Signature string
	// Data is the opaque payload of a "redacted_thinking" block, replayed
	// verbatim.
	Data string
}

// ToolCall represents a function call the LLM wants to make.
type ToolCall struct {
	// ID is the unique identifier for this tool call.
	ID string `json:"id"`
	// Type is the tool call type (always "function").
	Type string `json:"type"`
	// Function contains the function name and arguments.
	Function FunctionCall `json:"function"`
}

// FunctionCall is the name and arguments of a tool call.
type FunctionCall struct {
	// Name is the function name to call.
	Name string `json:"name"`
	// Arguments is the raw JSON string of function arguments.
	Arguments string `json:"arguments"`
}

// Usage holds token consumption data from a single LLM call.
// Populated from the provider's response when available.
//
// Field semantics are NORMALIZED across providers: regardless of
// whether the upstream API returns a gross input_tokens (OpenAI) or
// a fresh-only input_tokens (Anthropic), adapters populate
// PromptTokens with the FRESH (uncached) portion and CachedTokens
// with the cached subset. The two are disjoint. Total per-turn
// input = PromptTokens + CachedTokens.
//
// This matches opencode's `tokens.input` and pi's `input` so the
// per-turn footprint formula (input + cached + output [+
// cache_write + reasoning]) is directly comparable across the
// three tools.
type Usage struct {
	// PromptTokens is the number of FRESH (uncached) input tokens
	// — the portion paid for at the full per-token rate on this
	// turn. Disjoint from CachedTokens.
	PromptTokens int
	// CompletionTokens is the number of output tokens generated.
	CompletionTokens int
	// CachedTokens is the number of input tokens served from cache
	// at the discounted rate. Disjoint from PromptTokens. Zero
	// when caching is not active.
	CachedTokens int
	// CacheWriteTokens is the number of input tokens written to the
	// prompt cache on this turn (Anthropic cache_creation_input_tokens).
	// Billed at a premium (~1.25x the fresh input rate) over PromptTokens,
	// and disjoint from both PromptTokens and CachedTokens. Zero when no
	// cache write occurred or the provider does not report it.
	CacheWriteTokens int
}

// StreamEvent is one chunk from the LLM stream.
type StreamEvent struct {
	// Token is the text delta (may be empty on the final event).
	Token string
	// ToolCalls are the accumulated tool calls (populated when Done is true).
	ToolCalls []ToolCall
	// Reasoning is the assistant turn's reasoning trace, populated when Done
	// is true for providers that produce one (else nil). The foundation
	// copies it onto the assembled assistant [Message.Reasoning], mirroring
	// how it copies ToolCalls.
	Reasoning *ReasoningTrace
	// Done is true when the stream is complete.
	Done bool
	// Usage holds token consumption data, populated on the final event
	// when the provider reports usage. Nil when unavailable.
	Usage *Usage
	// Truncated is true when the provider stopped mid-generation because
	// the output token limit was reached. Any accumulated ToolCalls on a
	// truncated event may have incomplete arguments and must not be
	// executed — silently applying a truncated edit corrupts the file.
	Truncated bool
	// Err carries a provider-side stream failure as a terminal event:
	// a streamed error block, a payload-limit breach, or a mid-stream
	// stall. When non-nil this is the final event (Done is also set) and
	// any accumulated content, tool calls, or usage may be partial.
	// Consumers must surface Err instead of treating the turn as a clean
	// completion. Nil on success.
	Err error
}

// CacheControl marks a message or tool definition for provider-level prompt
// caching. Supported by Anthropic models via OpenRouter — cached input tokens
// cost ~90% less on subsequent requests with the same prefix.
type CacheControl struct {
	// Type is the cache control type (always "ephemeral").
	Type string `json:"type"`
}

// sseMaxLineBytes caps a single SSE line. Large file content in tool
// results can push individual data lines into the megabytes.
const sseMaxLineBytes = 10 * 1024 * 1024

// maxToolArgBytes is the maximum cumulative size of streamed tool call
// arguments. Matches sseMaxLineBytes to prevent unbounded growth.
const maxToolArgBytes = sseMaxLineBytes

// maxToolCalls is the maximum number of concurrent tool calls in a single
// response. Prevents unbounded slice/map growth from malformed SSE payloads.
const maxToolCalls = 128

// maxThinkingBytes bounds a captured extended-thinking block. Generous
// (well above any real reasoning budget) but finite so a malformed or
// runaway stream cannot exhaust memory.
const maxThinkingBytes = 2 * 1024 * 1024

// sseIdleTimeout caps the wait between consecutive SSE chunks. Normal slow
// generation streams tokens well within this window; a longer gap means the
// connection has stalled mid-stream, so the watchdog tears the body down to
// fail the turn instead of hanging it forever. Generous enough not to kill a
// model that pauses to think between tokens. A var so tests can shorten it.
var sseIdleTimeout = 60 * time.Second

// streamWatchdog closes the response body when no SSE chunk has arrived
// within sseIdleTimeout, converting a silent mid-stream stall into a prompt
// read error. Callers reset it after every successful read and stop it on
// return; fired reports whether the timeout (not a normal close) triggered.
type streamWatchdog struct {
	timer   *time.Timer
	stalled atomic.Bool
}

// newStreamWatchdog starts a watchdog that closes body after sseIdleTimeout
// of inactivity. The returned watchdog must be stopped by the caller.
func newStreamWatchdog(body io.Closer) *streamWatchdog {
	w := &streamWatchdog{}
	w.timer = time.AfterFunc(sseIdleTimeout, func() {
		w.stalled.Store(true)
		_ = body.Close() // unblock the blocked read; close error is not actionable
	})
	return w
}

// reset restarts the idle countdown after a chunk is read. Once the
// watchdog has fired the body is closed; re-arming would only schedule a
// second Close, so buffered lines drain without touching the timer.
func (w *streamWatchdog) reset() {
	if w.stalled.Load() {
		return
	}
	w.timer.Reset(sseIdleTimeout)
}

// stop halts the watchdog. Safe to call after it has fired.
func (w *streamWatchdog) stop() { w.timer.Stop() }

// fired reports whether the watchdog closed the body due to a stall.
func (w *streamWatchdog) fired() bool { return w.stalled.Load() }

// errStreamStalled is the terminal error surfaced when the watchdog tears
// down a stalled stream.
var errStreamStalled = fmt.Errorf("llm: stream stalled (no data for %s)", sseIdleTimeout)

// trySend sends an event on ch, returning false if ctx is cancelled.
// Prevents SSE goroutines from blocking indefinitely when the downstream
// reader stalls or abandons the stream.
func trySend(ctx context.Context, ch chan<- StreamEvent, evt StreamEvent) bool {
	select {
	case ch <- evt:
		return true
	case <-ctx.Done():
		return false
	}
}

// openStream sends req and returns the response once the status is 200.
// A transport failure or non-200 status becomes an error prefixed with
// provider; see [handleErrorStatus] for the 401 self-heal.
func openStream(client *http.Client, req *http.Request, provider string, auth Auth) (*http.Response, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: http request: %w", provider, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, handleErrorStatus(provider, auth, resp)
	}
	return resp, nil
}

// handleErrorStatus converts a non-200 response into an error, reading a
// bounded prefix of the body for detail and closing it. A 401 becomes a
// typed [AuthError] via [authFailure] regardless of body readability —
// the status alone is authoritative for the credential self-heal.
func handleErrorStatus(provider string, auth Auth, resp *http.Response) error {
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2048))
	_ = resp.Body.Close() // body drained above; close error is not actionable
	if resp.StatusCode == http.StatusUnauthorized {
		if readErr != nil {
			body = nil
		}
		return authFailure(provider, auth, resp.StatusCode, body)
	}
	if readErr != nil {
		return fmt.Errorf("%s: api error: status %d (body unreadable: %w)", provider, resp.StatusCode, readErr)
	}
	return fmt.Errorf("%s: api error: status %d: %s", provider, resp.StatusCode, errorDetail(body))
}

// maxErrorDetailBytes bounds the free-text detail surfaced from a non-JSON
// provider error body.
const maxErrorDetailBytes = 256

// errorDetail extracts the human-readable message from a provider error
// body. JSON envelopes ({"error":{"message":..}} or {"error":".."}) yield
// only their message field, never the raw payload, since some providers
// echo request details there. Anything else is surfaced as a bounded
// single-line snippet.
func errorDetail(body []byte) string {
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && len(env.Error) > 0 {
		var obj struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(env.Error, &obj) == nil && obj.Message != "" {
			return obj.Message
		}
		var msg string
		if json.Unmarshal(env.Error, &msg) == nil && msg != "" {
			return msg
		}
		return "(json error body without message)"
	}
	s := strings.Join(strings.Fields(string(body)), " ")
	if len(s) > maxErrorDetailBytes {
		s = s[:maxErrorDetailBytes] + "…"
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}

// authFailure handles a 401 from any provider. Auth already refreshed
// during Authenticate, so a 401 means a dead/revoked credential: discard
// it through the optional [CredentialInvalidator] port (a no-op for static
// keys) so the next launch re-offers connect, and return a typed
// [AuthError] instead of the raw body.
func authFailure(provider string, auth Auth, statusCode int, body []byte) error {
	if inv, ok := auth.(CredentialInvalidator); ok {
		inv.Invalidate()
	}
	return parseAuthError(provider, statusCode, body)
}

// startSSEReader runs read over resp.Body on its own goroutine and returns
// the event channel it feeds. Body and channel are closed when read
// returns, so consumers can range until close.
func startSSEReader(ctx context.Context, resp *http.Response, read func(ctx context.Context, body io.ReadCloser, ch chan<- StreamEvent)) <-chan StreamEvent {
	ch := make(chan StreamEvent, 16)
	go func() {
		defer func() { _ = resp.Body.Close() }() // SSE stream done; close error is not actionable
		defer close(ch)
		read(ctx, resp.Body, ch)
	}()
	return ch
}

// scanSSE drives the shared SSE read loop: it feeds every "data:" line
// (with the preceding "event:" type, if any) to onEvent until onEvent
// reports stop, ctx is cancelled, the body ends, or a read fails. Clean
// EOF without a stop emits terminal(); a watchdog stall emits
// errStreamStalled; any other read error is logged and nothing is
// emitted, so the consumer sees a provider-side failure rather than a
// synthesized success with possibly partial tool calls.
func scanSSE(ctx context.Context, body io.ReadCloser, ch chan<- StreamEvent, provider string, onEvent func(eventType, data string) (stop bool), terminal func() StreamEvent) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), sseMaxLineBytes)

	wd := newStreamWatchdog(body)
	defer wd.stop()

	var eventType string
	for scanner.Scan() {
		wd.reset()
		if ctx.Err() != nil {
			return
		}
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if line == "" {
			eventType = "" // blank line ends the event; the type does not carry over
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		stop := onEvent(eventType, data)
		eventType = ""
		if stop {
			return
		}
	}

	// A fired watchdog closed the body; the scanner may still have drained
	// buffered lines to a clean EOF, which must not read as success.
	if wd.fired() {
		trySend(ctx, ch, StreamEvent{Done: true, Err: errStreamStalled})
		return
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() == nil {
			slog.Warn("SSE scanner error", "provider", provider, "err", err)
		}
		return
	}
	trySend(ctx, ch, terminal())
}

// OutputCapEscalator is an optional provider capability: the agent raises
// the output-token cap after a truncated turn and retries. Every provider
// in this package implements it; foreign providers may not, so callers
// type-assert.
type OutputCapEscalator interface {
	// MaxTokens returns the current output-token cap. Zero means the
	// field is omitted and the provider default applies.
	MaxTokens() int
	// SetMaxTokens sets the cap used on subsequent requests. Zero
	// restores the provider default where the API allows it.
	SetMaxTokens(int)
}

// maxTokensState is the mutex-guarded output-token cap embedded by every
// provider adapter to satisfy [OutputCapEscalator].
type maxTokensState struct {
	mu        sync.Mutex
	maxTokens int
}

// MaxTokens returns the current output-token cap. Safe for concurrent use.
func (s *maxTokensState) MaxTokens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxTokens
}

// SetMaxTokens updates the cap used on subsequent requests. Safe for
// concurrent use.
func (s *maxTokensState) SetMaxTokens(v int) {
	s.mu.Lock()
	s.maxTokens = v
	s.mu.Unlock()
}

// Provider abstracts an LLM backend for streaming chat completions.
//
// Implementations should pass [github.com/latebit-io/nib/kit/contracttest.Provider]
// — the fixture verifies channel closure on completion and cancel,
// no-events-after-Done, Truncated-implies-Done, and concurrent-Stream
// safety against any conforming implementation.
type Provider interface {
	// Stream sends messages with the given tool definitions and returns
	// a channel of streaming events. The channel is closed when the
	// response is complete or ctx is cancelled.
	Stream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamEvent, error)
}

// Effort is a provider-agnostic reasoning-effort level. Providers map it
// to their own reasoning control (OpenAI-style reasoning_effort, Anthropic
// extended-thinking budget). The empty value means "unset" — no reasoning
// field is sent and the provider's default applies. Effort is fixed at
// provider construction, like the model, since [Provider.Stream] takes no
// per-call parameters.
type Effort string

// Effort levels, mirroring the set an agent definition may request.
const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
	EffortMax    Effort = "max"
)

// IsValid reports whether e is a recognized effort level, treating the
// empty value (unset) as valid. Call sites that cast an arbitrary config
// string into [Effort] can use it to fail fast on a typo (e.g. "hihg")
// instead of silently degrading to the provider default, which is what
// the providers' internal mapping does for an unrecognized value.
func (e Effort) IsValid() bool {
	switch e {
	case "", EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax:
		return true
	default:
		return false
	}
}

// openAIEffort maps an [Effort] to an OpenAI reasoning-effort value
// (low|medium|high). nib's higher tiers (xhigh, max) collapse to "high",
// the ceiling the OpenAI reasoning API accepts. Returns "" for an unset or
// unrecognized effort so the caller omits the field entirely.
func openAIEffort(e Effort) string {
	switch e {
	case EffortLow:
		return "low"
	case EffortMedium:
		return "medium"
	case EffortHigh, EffortXHigh, EffortMax:
		return "high"
	default:
		return ""
	}
}
