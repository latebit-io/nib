package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// fakeInvalidatableAuth records whether Invalidate was called, so tests
// can assert the 401 self-heal fired. It satisfies both Auth and
// CredentialInvalidator.
type fakeInvalidatableAuth struct{ invalidated bool }

func (f *fakeInvalidatableAuth) Authenticate(context.Context, *http.Request) error { return nil }
func (f *fakeInvalidatableAuth) Invalidate()                                       { f.invalidated = true }

func TestCodexAPI_AuthFailureInvalidatesAndReturnsTypedError(t *testing.T) {
	auth := &fakeInvalidatableAuth{}
	body := []byte(`{"error":{"message":"Encountered invalidated oauth token","code":"token_revoked"},"status":401}`)
	err := authFailure(codexProviderName, auth, http.StatusUnauthorized, body)

	if !auth.invalidated {
		t.Error("authFailure did not call Invalidate on the credential")
	}
	var ae *AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("authFailure returned %T, want *AuthError", err)
	}
	if ae.Code != "token_revoked" {
		t.Errorf("AuthError.Code = %q, want token_revoked", ae.Code)
	}
}

// staticAuth satisfies Auth but NOT CredentialInvalidator — a 401 against
// API-key auth must not panic and must still return a typed AuthError.
type staticAuth struct{}

func (staticAuth) Authenticate(context.Context, *http.Request) error { return nil }

func TestCodexAPI_AuthFailureNoInvalidatorStillTypesError(t *testing.T) {
	err := authFailure(codexProviderName, staticAuth{}, http.StatusUnauthorized, nil)

	var ae *AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("authFailure returned %T, want *AuthError", err)
	}
	if ae.StatusCode != http.StatusUnauthorized {
		t.Errorf("AuthError.StatusCode = %d, want 401", ae.StatusCode)
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCodexAPI_ListModelsSkipsHiddenAndSendsClientVersion(t *testing.T) {
	c := NewCodexAPI("", staticAuth{}, "")
	var gotVersion string
	c.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotVersion = r.URL.Query().Get("client_version")
		body := `{"models":[
			{"slug":"gpt-6.1-sol","display_name":"GPT-6.1-Sol","visibility":"list"},
			{"slug":"gpt-reserve","display_name":"GPT-Reserve","visibility":"hide"},
			{"slug":"gpt-6-luna","visibility":"list"}]}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}

	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotVersion != codexClientVersion {
		t.Errorf("client_version = %q, want %q; the endpoint lists nothing without a current one", gotVersion, codexClientVersion)
	}
	want := []ModelInfo{{ID: "gpt-6.1-sol", Name: "GPT-6.1-Sol"}, {ID: "gpt-6-luna", Name: "gpt-6-luna"}}
	if !slices.Equal(models, want) {
		t.Errorf("models = %+v, want %+v", models, want)
	}
}

// Codex SSE fixtures shared by the stream tests.
const (
	codexTextDelta = `{"type":"response.output_text.delta","delta":"hi"}`
	codexToolAdded = `{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"c1","name":"f"}}`
	codexToolDone  = `{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}}`
	codexCompleted = `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":2}}}`
	// codexSchemaError is the frame reported in issue #226: an invalid
	// tool schema rejected inside an HTTP-200 stream.
	codexSchemaError     = `{"type":"error","error":{"type":"invalid_request_error","code":"invalid_function_parameters","param":"tools","message":"Invalid schema for function application_settings"}}`
	codexSchemaErrorWant = "chatgpt: stream error: invalid_function_parameters (param tools): Invalid schema for function application_settings"
	codexStreamEndedWant = "chatgpt: stream ended before completion"
)

// codexIncomplete is a response.incomplete frame; an empty reason omits
// incomplete_details.
func codexIncomplete(reason string) string {
	details := ""
	if reason != "" {
		details = `,"incomplete_details":{"reason":"` + reason + `"}`
	}
	return `{"type":"response.incomplete","response":{"status":"incomplete"` + details + `}}`
}

// collectStream drains ch until the provider closes it.
func collectStream(ch <-chan StreamEvent) []StreamEvent {
	var evs []StreamEvent
	for ev := range ch {
		evs = append(evs, ev)
	}
	return evs
}

// codexStream runs frames through the public CodexAPI.Stream behind an
// HTTP-200 event-stream response and returns every event emitted.
func codexStream(t *testing.T, frames ...string) []StreamEvent {
	t.Helper()
	var sse strings.Builder
	for _, f := range frames {
		sse.WriteString("data: " + f + "\n\n")
	}
	c := NewCodexAPI("m", staticAuth{}, "")
	c.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sse.String())),
		}, nil
	})}
	ch, err := c.Stream(context.Background(), []Message{{Role: "user", Content: "x"}}, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	return collectStream(ch)
}

// terminalOf asserts evs ends with exactly one Done event and returns it,
// with the number of text tokens streamed before it.
func terminalOf(t *testing.T, evs []StreamEvent) (last StreamEvent, tokens int) {
	t.Helper()
	if len(evs) == 0 || !evs[len(evs)-1].Done {
		t.Fatalf("events = %+v; want a terminal Done event last", evs)
	}
	for _, ev := range evs[:len(evs)-1] {
		if ev.Done || ev.Err != nil {
			t.Fatalf("non-final event %+v is terminal", ev)
		}
		if ev.Token != "" {
			tokens++
		}
	}
	return evs[len(evs)-1], tokens
}

// TestCodexAPI_StreamSurfacesProviderErrors: failures the provider reports
// inside an HTTP-200 stream, and streams cut before a terminal response
// event, end in a terminal Err rather than an empty successful completion,
// even after a text or tool-call prefix (issue #226).
func TestCodexAPI_StreamSurfacesProviderErrors(t *testing.T) {
	cases := []struct {
		name       string
		frames     []string
		wantErr    string
		wantTokens int
	}{
		{"nested error event", []string{codexSchemaError}, codexSchemaErrorWant, 0},
		{"flat error event", []string{`{"type":"error","code":"rate_limit_exceeded","message":"slow down","param":null,"sequence_number":3}`},
			"chatgpt: stream error: rate_limit_exceeded: slow down", 0},
		{"response.failed", []string{`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"The model failed"}}}`},
			"chatgpt: response failed: server_error: The model failed", 0},
		{"response.failed without detail", []string{`{"type":"response.failed","response":{"status":"failed"}}`},
			"chatgpt: response failed: (no message)", 0},
		{"text and tool prefix then error", []string{codexTextDelta, codexToolAdded, codexToolDone, codexSchemaError}, codexSchemaErrorWant, 1},
		{"incomplete for content_filter", []string{codexTextDelta, codexIncomplete("content_filter")},
			"chatgpt: response incomplete: content_filter", 1},
		{"incomplete without reason", []string{codexIncomplete("")}, "chatgpt: response incomplete: no reason given", 0},
		{"EOF before terminal event", []string{codexTextDelta, codexToolAdded, codexToolDone}, codexStreamEndedWant, 1},
		{"[DONE] before terminal event", []string{codexTextDelta, "[DONE]"}, codexStreamEndedWant, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			last, tokens := terminalOf(t, codexStream(t, tc.frames...))
			if last.Err == nil || last.Err.Error() != tc.wantErr {
				t.Fatalf("terminal Err = %v; want %q", last.Err, tc.wantErr)
			}
			if len(last.ToolCalls) != 0 {
				t.Errorf("ToolCalls = %+v; a failed turn must not hand back calls to execute", last.ToolCalls)
			}
			if tokens != tc.wantTokens {
				t.Errorf("tokens = %d; want %d", tokens, tc.wantTokens)
			}
		})
	}
}

// TestCodexAPI_StreamValidCompletions: text, tool-call, and output-cap
// completions keep their successful terminal events.
func TestCodexAPI_StreamValidCompletions(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		last, tokens := terminalOf(t, codexStream(t, codexTextDelta, codexCompleted))
		if last.Err != nil || last.Truncated || tokens != 1 {
			t.Fatalf("terminal = %+v, tokens = %d; want clean Done after 1 token", last, tokens)
		}
		if last.Usage == nil || last.Usage.PromptTokens != 10 || last.Usage.CompletionTokens != 2 {
			t.Errorf("Usage = %+v; want 10 prompt, 2 completion", last.Usage)
		}
	})
	t.Run("tool call", func(t *testing.T) {
		last, _ := terminalOf(t, codexStream(t, codexToolAdded, codexToolDone, codexCompleted))
		want := []ToolCall{{ID: "c1", Type: "function", Function: FunctionCall{Name: "f", Arguments: "{}"}}}
		if last.Err != nil || !slices.Equal(last.ToolCalls, want) {
			t.Fatalf("terminal = %+v; want clean Done with %+v", last, want)
		}
	})
	t.Run("max_output_tokens", func(t *testing.T) {
		last, _ := terminalOf(t, codexStream(t, codexTextDelta, codexIncomplete("max_output_tokens")))
		if last.Err != nil || !last.Truncated {
			t.Fatalf("terminal = %+v; want Truncated without Err so the agent can escalate", last)
		}
	})
}
