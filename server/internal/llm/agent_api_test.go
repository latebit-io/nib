package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAgentAPIStream(t *testing.T) {
	// Mock SSE server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("expected auth header, got %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected json content type")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected flusher")
		}

		events := []string{
			`data: {"choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{"content":" world"},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{"content":""},"finish_reason":"stop"}]}`,
		}
		for _, e := range events {
			_, _ = fmt.Fprintf(w, "%s\n\n", e)
			flusher.Flush()
		}
	}))
	defer srv.Close()

	m := &AgentAPI{
		APIKey:  "test-key",
		BaseURL: srv.URL,
		Model:   "test-model",
		client:  srv.Client(),
	}

	ch, err := m.Stream(context.Background(), []Message{
		{Role: "user", Content: "hi"},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var tokens []string
	var gotDone bool
	for ev := range ch {
		if ev.Done {
			gotDone = true
			break
		}
		tokens = append(tokens, ev.Token)
	}

	if !gotDone {
		t.Fatal("expected done event")
	}
	if len(tokens) != 2 {
		t.Fatalf("expected 2 tokens, got %d: %v", len(tokens), tokens)
	}
	if tokens[0] != "Hello" || tokens[1] != " world" {
		t.Fatalf("unexpected tokens: %v", tokens)
	}
}

func TestAgentAPIStreamDoneMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	m := &AgentAPI{
		APIKey:  "test-key",
		BaseURL: srv.URL,
		client:  srv.Client(),
	}

	ch, err := m.Stream(context.Background(), []Message{
		{Role: "user", Content: "test"},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var tokens []string
	var gotDone bool
	for ev := range ch {
		if ev.Done {
			gotDone = true
			break
		}
		tokens = append(tokens, ev.Token)
	}

	if !gotDone {
		t.Fatal("expected done event")
	}
	if len(tokens) != 1 || tokens[0] != "hi" {
		t.Fatalf("unexpected tokens: %v", tokens)
	}
}

func TestAgentAPIStreamAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	m := &AgentAPI{
		APIKey:  "bad-key",
		BaseURL: srv.URL,
		client:  srv.Client(),
	}

	_, err := m.Stream(context.Background(), []Message{
		{Role: "user", Content: "test"},
	})
	if err == nil {
		t.Fatal("expected error for 401")
	}
}

func TestAgentAPIStreamToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected flusher")
		}
		events := []string{
			// Reasoning text
			`data: {"choices":[{"delta":{"content":"I'll fix this."},"finish_reason":null}]}`,
			// Tool call: first chunk with ID and name
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"edit_file","arguments":""}}]},"finish_reason":null}]}`,
			// Tool call: argument chunks
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"search\":\"old"}}]},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\",\"replace\":\"new\",\"reason\":\"fix\"}"}}]},"finish_reason":null}]}`,
			// Done
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		}
		for _, e := range events {
			_, _ = fmt.Fprintf(w, "%s\n\n", e)
			flusher.Flush()
		}
	}))
	defer srv.Close()

	m := &AgentAPI{
		APIKey:  "test-key",
		BaseURL: srv.URL,
		Model:   "test-model",
		client:  srv.Client(),
	}

	ch, err := m.Stream(context.Background(), []Message{
		{Role: "user", Content: "fix the bug"},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var tokens []string
	var toolCalls []ToolCall
	for ev := range ch {
		if ev.Done {
			toolCalls = ev.ToolCalls
			break
		}
		if ev.Token != "" {
			tokens = append(tokens, ev.Token)
		}
	}

	if len(tokens) != 1 || tokens[0] != "I'll fix this." {
		t.Fatalf("unexpected tokens: %v", tokens)
	}
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}
	tc := toolCalls[0]
	if tc.ID != "call_abc" {
		t.Fatalf("expected tool call ID call_abc, got %s", tc.ID)
	}
	if tc.Function.Name != "edit_file" {
		t.Fatalf("expected function name edit_file, got %s", tc.Function.Name)
	}
	expected := `{"search":"old","replace":"new","reason":"fix"}`
	if tc.Function.Arguments != expected {
		t.Fatalf("expected arguments %q, got %q", expected, tc.Function.Arguments)
	}
}

func TestAgentAPIStreamCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		// Send one event then hang
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
		flusher.Flush()
		// Block until client disconnects
		<-r.Context().Done()
	}))
	defer srv.Close()

	m := &AgentAPI{
		APIKey:  "test-key",
		BaseURL: srv.URL,
		client:  srv.Client(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := m.Stream(ctx, []Message{
		{Role: "user", Content: "test"},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Read first token
	ev := <-ch
	if ev.Token != "hi" {
		t.Fatalf("expected 'hi', got %q", ev.Token)
	}

	// Cancel and verify channel closes
	cancel()
	for range ch {
		// drain
	}
	// If we get here, channel was closed properly
}
