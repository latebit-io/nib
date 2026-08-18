package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
)

// errBody yields data, then fails the read — a connection drop mid-stream.
func errBody(data string, err error) io.ReadCloser {
	return io.NopCloser(io.MultiReader(strings.NewReader(data), iotest.ErrReader(err)))
}

// TestCodexSSE_ScannerErrorEmitsNoDone: a read failure with ctx live must
// close the channel WITHOUT a Done event. Synthesizing Done here would hand
// the agent possibly partial tool calls as a clean completion.
func TestCodexSSE_ScannerErrorEmitsNoDone(t *testing.T) {
	c := NewCodexAPI("m", staticAuth{}, "")
	body := errBody(
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n"+
			"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"f\"}}\n",
		errors.New("connection reset"))
	ch := make(chan StreamEvent, 8)
	c.readCodexSSE(context.Background(), body, ch)
	close(ch)

	var tokens int
	for ev := range ch {
		if ev.Done {
			t.Fatalf("scanner error emitted Done event %+v; want none", ev)
		}
		tokens++
	}
	if tokens != 1 {
		t.Errorf("tokens = %d; want 1 (delta before the failure)", tokens)
	}
}

// TestScanSSE_CleanEOFEmitsTerminal covers the shared loop's fallback: no
// stop from the dispatcher, body ends cleanly, terminal() is emitted once.
func TestScanSSE_CleanEOFEmitsTerminal(t *testing.T) {
	body := io.NopCloser(strings.NewReader("event: ping\ndata: a\n\ndata: b\n"))
	ch := make(chan StreamEvent, 4)
	var seen []string
	scanSSE(context.Background(), body, ch, "test", func(eventType, data string) bool {
		seen = append(seen, eventType+"/"+data)
		return false
	}, func() StreamEvent { return StreamEvent{Done: true} })
	close(ch)

	if strings.Join(seen, ",") != "ping/a,ping/b" {
		t.Errorf("dispatched = %v; want [ping/a ping/b]", seen)
	}
	var dones int
	for ev := range ch {
		if ev.Done {
			dones++
		}
	}
	if dones != 1 {
		t.Errorf("Done events = %d; want 1", dones)
	}
}

// TestStream_401ReturnsAuthErrorAndInvalidates: every HTTP adapter must
// turn a 401 into a typed AuthError and discard the credential through
// the CredentialInvalidator port, not just Codex.
func TestStream_401ReturnsAuthErrorAndInvalidates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key","code":"invalid_api_key"}}`)) // test server; write error irrelevant
	}))
	defer srv.Close()

	cases := []struct {
		name string
		mk   func(auth Auth) Provider
		want string // AuthError.Provider
	}{
		{"agent", func(a Auth) Provider { return NewAgentAPI(srv.URL, "m", a, false, "") }, agentAPIProviderName},
		{"anthropic", func(a Auth) Provider { return NewAnthropicAPI(srv.URL, "m", a, false, "") }, anthropicProviderName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := &fakeInvalidatableAuth{}
			_, err := tc.mk(auth).Stream(context.Background(), []Message{{Role: "user", Content: "x"}}, nil)
			var ae *AuthError
			if !errors.As(err, &ae) {
				t.Fatalf("Stream error = %v (%T); want *AuthError", err, err)
			}
			if ae.Provider != tc.want || ae.Code != "invalid_api_key" || ae.StatusCode != http.StatusUnauthorized {
				t.Errorf("AuthError = %+v; want Provider=%s Code=invalid_api_key Status=401", ae, tc.want)
			}
			if !auth.invalidated {
				t.Error("401 did not invalidate the credential")
			}
		})
	}
}

// TestStream_Non401ErrorIsPlain: a 500 stays a plain error carrying the
// body, and never touches the credential.
func TestStream_Non401ErrorIsPlain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	auth := &fakeInvalidatableAuth{}
	_, err := NewAgentAPI(srv.URL+"/", "m", auth, false, "").Stream(context.Background(), []Message{{Role: "user", Content: "x"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "status 500") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Stream error = %v; want status 500 with body", err)
	}
	var ae *AuthError
	if errors.As(err, &ae) {
		t.Error("500 must not be an AuthError")
	}
	if auth.invalidated {
		t.Error("500 must not invalidate the credential")
	}
}
