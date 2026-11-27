package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// openResponsesStream is a spec-shaped Open Responses stream (2026-04-24):
// a message and a function call framed by their added and done events,
// an extension event on the way, and no [DONE] after the terminal one.
var openResponsesStream = []string{
	`{"type":"response.created","sequence_number":0,"response":{"id":"r1","status":"in_progress"}}`,
	`{"type":"response.in_progress","sequence_number":1,"response":{"id":"r1","status":"in_progress"}}`,
	`{"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"type":"message","id":"m1","role":"assistant","status":"in_progress","content":[]}}`,
	`{"type":"response.content_part.added","sequence_number":3,"item_id":"m1","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
	`{"type":"response.output_text.delta","sequence_number":4,"item_id":"m1","output_index":0,"content_index":0,"delta":"Hel"}`,
	`{"type":"acme:trace_event","sequence_number":5,"detail":"an extension event a client must ignore"}`,
	`{"type":"response.output_text.delta","sequence_number":6,"item_id":"m1","output_index":0,"content_index":0,"delta":"lo"}`,
	`{"type":"response.output_text.done","sequence_number":7,"item_id":"m1","output_index":0,"content_index":0,"text":"Hello"}`,
	`{"type":"response.content_part.done","sequence_number":8,"item_id":"m1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"Hello"}}`,
	`{"type":"response.output_item.done","sequence_number":9,"output_index":0,"item":{"type":"message","id":"m1","role":"assistant","status":"completed"}}`,
	`{"type":"response.output_item.added","sequence_number":10,"output_index":1,"item":{"type":"function_call","id":"fc1","call_id":"call_1","name":"lookup","arguments":"","status":"in_progress"}}`,
	`{"type":"response.function_call_arguments.delta","sequence_number":11,"item_id":"fc1","output_index":1,"delta":"{\"q\":"}`,
	`{"type":"response.function_call_arguments.delta","sequence_number":12,"item_id":"fc1","output_index":1,"delta":"\"x\"}"}`,
	`{"type":"response.function_call_arguments.done","sequence_number":13,"item_id":"fc1","output_index":1,"arguments":"{\"q\":\"x\"}"}`,
	`{"type":"response.output_item.done","sequence_number":14,"output_index":1,"item":{"type":"function_call","id":"fc1","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}","status":"completed"}}`,
	`{"type":"response.completed","sequence_number":15,"response":{"id":"r1","status":"completed","usage":{"input_tokens":12,"output_tokens":5,"total_tokens":17,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":0}}}}`,
}

// responsesServer serves frames as an Open Responses SSE stream, with
// each event's name matching its type, and records the request.
func responsesServer(t *testing.T, frames []string) (*httptest.Server, *http.Request, *[]byte) {
	t.Helper()
	var got http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r.Clone(context.Background())
		var err error
		if body, err = io.ReadAll(r.Body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			var head struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(f), &head); err != nil {
				t.Fatalf("bad fixture %s: %v", f, err)
			}
			if _, err := io.WriteString(w, "event: "+head.Type+"\ndata: "+f+"\n\n"); err != nil {
				t.Error(err)
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &body
}

// TestResponsesAPI_SpecStream: the provider follows the spec's client
// rules: a JSON body to <base>/responses, a stream that ends without
// [DONE] accepted at its terminal event, unknown events ignored.
func TestResponsesAPI_SpecStream(t *testing.T) {
	srv, req, body := responsesServer(t, openResponsesStream)
	r := NewResponsesAPI(srv.URL+"/v1/", "open-model", StaticKeyAuth("k"), "")
	tools := []ToolDef{{Type: "function", Function: FunctionDef{Name: "lookup", Description: "Looks up", Parameters: FunctionParams{Type: "object"}}}}
	ch, err := r.Stream(context.Background(), []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_0", Type: "function", Function: FunctionCall{Name: "lookup", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "call_0", Content: "found"},
	}, tools)
	if err != nil {
		t.Fatal(err)
	}
	last, tokens := terminalOf(t, collectStream(ch))

	if req.URL.Path != "/v1/responses" || req.Header.Get("Content-Type") != "application/json" || req.Header.Get("Authorization") != "Bearer k" {
		t.Errorf("request = %s %s, headers %v", req.Method, req.URL.Path, req.Header)
	}
	var sent struct {
		Model        string            `json:"model"`
		Instructions *string           `json:"instructions"`
		Stream       bool              `json:"stream"`
		Store        *bool             `json:"store"`
		Input        []json.RawMessage `json:"input"`
		Tools        []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(*body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Model != "open-model" || !sent.Stream || sent.Store == nil || *sent.Store || sent.Instructions != nil || len(sent.Input) != 3 || len(sent.Tools) != 1 {
		t.Errorf("body = %s", *body)
	}
	for i, want := range []string{`"type":"message"`, `"type":"function_call"`, `"type":"function_call_output"`} {
		if !strings.Contains(string(sent.Input[i]), want) {
			t.Errorf("input[%d] = %s, want %s", i, sent.Input[i], want)
		}
	}

	if tokens != 2 || last.Truncated {
		t.Errorf("tokens = %d, truncated = %v", tokens, last.Truncated)
	}
	if len(last.ToolCalls) != 1 || last.ToolCalls[0].ID != "call_1" || last.ToolCalls[0].Function.Arguments != `{"q":"x"}` {
		t.Errorf("tool calls = %+v", last.ToolCalls)
	}
	if u := last.Usage; u == nil || u.PromptTokens != 8 || u.CachedTokens != 4 || u.CompletionTokens != 5 {
		t.Errorf("usage = %+v, want 8 fresh + 4 cached in, 5 out", last.Usage)
	}
}

// TestResponsesAPI_InstructionsAndEffort: the first system message is
// the instructions, later ones developer messages, and the effort is sent.
func TestResponsesAPI_InstructionsAndEffort(t *testing.T) {
	srv, _, body := responsesServer(t, openResponsesStream[len(openResponsesStream)-1:])
	r := NewResponsesAPI(srv.URL, "m", StaticKeyAuth("k"), EffortHigh)
	r.SetMaxTokens(2048)
	ch, err := r.Stream(context.Background(), []Message{
		{Role: "system", Content: "be brief"}, {Role: "system", Content: "also kind"}, {Role: "user", Content: "x"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	collectStream(ch)
	for _, want := range []string{`"instructions":"be brief"`, `"role":"developer"`, `"effort":"high"`, `"max_output_tokens":2048`} {
		if !strings.Contains(string(*body), want) {
			t.Errorf("body lacks %s: %s", want, *body)
		}
	}
}
