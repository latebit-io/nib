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
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
}

// NewAgentAPI creates an AgentAPI provider with the given base URL, model, and API key.
func NewAgentAPI(baseURL, model, apiKey string) *AgentAPI {
	return &AgentAPI{
		apiKey:  apiKey,
		baseURL: baseURL,
		model:   model,
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

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
	Tools    []toolDef `json:"tools,omitempty"`
}

type sseChunk struct {
	Choices []struct {
		Delta struct {
			Content   string         `json:"content"`
			ToolCalls []sseDeltaCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
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
func (a *AgentAPI) Stream(ctx context.Context, messages []Message) (<-chan StreamEvent, error) {
	body, err := json.Marshal(chatRequest{
		Model:    a.model,
		Messages: messages,
		Stream:   true,
		Tools:    defaultTools,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	base := strings.TrimRight(a.baseURL, "/")
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.apiKey)

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

func (a *AgentAPI) readSSE(ctx context.Context, resp *http.Response, ch chan<- StreamEvent) {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024) // 10MB — large file content in tool results

	var tc toolCallAccumulator

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
			ch <- StreamEvent{Done: true, ToolCalls: tc.finalize()}
			return
		}

		var chunk sseChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			slog.Warn("SSE unmarshal error", "err", err, "data", data[:min(len(data), 200)])
			continue
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
			ch <- StreamEvent{Done: true, ToolCalls: tc.finalize()}
			return
		}
	}

	// Check for scanner errors (I/O failures, buffer overflow)
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		slog.Warn("SSE scanner error", "err", err)
	}

	// Stream ended without [DONE] or finish_reason (EOF or scanner error).
	if ctx.Err() == nil {
		ch <- StreamEvent{Done: true, ToolCalls: tc.finalize()}
	}
}
