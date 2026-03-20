package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// AgentAPI implements Provider using an OpenAI-compatible chat completions API.
type AgentAPI struct {
	APIKey  string
	BaseURL string
	Model   string
	client  *http.Client
}

// NewAgentAPI creates an AgentAPI provider with the given base URL, model, and API key.
func NewAgentAPI(baseURL, model, apiKey string) *AgentAPI {
	return &AgentAPI{
		APIKey:  apiKey,
		BaseURL: baseURL,
		Model:   model,
		client:  &http.Client{},
	}
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
	Tools    []ToolDef `json:"tools,omitempty"`
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
		Model:    a.Model,
		Messages: messages,
		Stream:   true,
		Tools:    DefaultTools,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", a.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.APIKey)

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
type toolCallAccumulator struct {
	calls []ToolCall
	args  []strings.Builder
}

func (tc *toolCallAccumulator) merge(deltas []sseDeltaCall) {
	for _, d := range deltas {
		// Grow slices if needed
		for d.Index >= len(tc.calls) {
			tc.calls = append(tc.calls, ToolCall{})
			tc.args = append(tc.args, strings.Builder{})
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
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

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

	// Stream ended without [DONE] or finish_reason (EOF or scanner error).
	if ctx.Err() == nil {
		ch <- StreamEvent{Done: true, ToolCalls: tc.finalize()}
	}
}
