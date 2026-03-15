package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const defaultBaseURL = "https://api.minimax.io/v1"

// MiniMax implements Provider using the MiniMax OpenAI-compatible API.
type MiniMax struct {
	APIKey  string
	BaseURL string // defaults to https://api.minimax.io/v1
	Model   string // defaults to MiniMax-M2.5
	client  *http.Client
}

// NewMiniMax creates a MiniMax provider with the given API key.
func NewMiniMax(apiKey string) *MiniMax {
	return &MiniMax{
		APIKey:  apiKey,
		BaseURL: defaultBaseURL,
		Model:   "MiniMax-M2.5",
		client:  &http.Client{},
	}
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []chatMsg `json:"messages"`
	Stream   bool      `json:"stream"`
}

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type sseChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

func (m *MiniMax) baseURL() string {
	if m.BaseURL != "" {
		return m.BaseURL
	}
	return defaultBaseURL
}

func (m *MiniMax) model() string {
	if m.Model != "" {
		return m.Model
	}
	return "MiniMax-M2.5"
}

// Stream sends a chat completion request and returns a channel of streaming events.
func (m *MiniMax) Stream(ctx context.Context, messages []Message) (<-chan StreamEvent, error) {
	msgs := make([]chatMsg, len(messages))
	for i, msg := range messages {
		msgs[i] = chatMsg(msg)
	}

	body, err := json.Marshal(chatRequest{
		Model:    m.model(),
		Messages: msgs,
		Stream:   true,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", m.baseURL()+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.APIKey)

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("api error: status %d", resp.StatusCode)
	}

	ch := make(chan StreamEvent, 16)
	go func() {
		defer func() { _ = resp.Body.Close() }()
		defer close(ch)
		m.readSSE(ctx, resp, ch)
	}()

	return ch, nil
}

func (m *MiniMax) readSSE(ctx context.Context, resp *http.Response, ch chan<- StreamEvent) {
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			ch <- StreamEvent{Done: true}
			return
		}

		var chunk sseChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		delta := chunk.Choices[0].Delta.Content
		if delta != "" {
			ch <- StreamEvent{Token: delta}
		}

		if chunk.Choices[0].FinishReason != nil {
			ch <- StreamEvent{Done: true}
			return
		}
	}
}
