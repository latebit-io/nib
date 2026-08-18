package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
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

	// The event type applies to the data line it precedes only; the
	// blank line (and each dispatch) resets it, per the SSE spec.
	if strings.Join(seen, ",") != "ping/a,/b" {
		t.Errorf("dispatched = %v; want [ping/a /b]", seen)
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

// blockingBody serves one chunk, then blocks every further Read until
// Close, after which it reports clean EOF — a stalled connection whose
// buffered lines are still in the scanner when the watchdog tears it down.
type blockingBody struct {
	chunk  string
	served bool
	closed chan struct{}
	once   sync.Once
}

func newBlockingBody(chunk string) *blockingBody {
	return &blockingBody{chunk: chunk, closed: make(chan struct{})}
}

func (b *blockingBody) Read(p []byte) (int, error) {
	if !b.served {
		b.served = true
		return copy(p, b.chunk), nil
	}
	<-b.closed
	return 0, io.EOF
}

func (b *blockingBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

// TestScanSSE_WatchdogFiredNeverEmitsTerminal: once the watchdog closes a
// stalled body, buffered lines draining to a clean EOF must surface
// errStreamStalled, not terminal() success — and must not re-arm the timer.
func TestScanSSE_WatchdogFiredNeverEmitsTerminal(t *testing.T) {
	prev := sseIdleTimeout
	sseIdleTimeout = 20 * time.Millisecond
	t.Cleanup(func() { sseIdleTimeout = prev })

	body := newBlockingBody("data: a\ndata: b\n")
	ch := make(chan StreamEvent, 4)
	var seen []string
	scanSSE(context.Background(), body, ch, "test", func(_, data string) bool {
		seen = append(seen, data)
		if data == "a" {
			// Stall inside the dispatcher until the watchdog fires and
			// closes the body; "b" is still buffered in the scanner.
			<-body.closed
		}
		return false
	}, func() StreamEvent { return StreamEvent{Done: true} })
	close(ch)

	if strings.Join(seen, ",") != "a,b" {
		t.Errorf("dispatched = %v; want [a b]", seen)
	}
	var got []StreamEvent
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) != 1 || !got[0].Done || !errors.Is(got[0].Err, errStreamStalled) {
		t.Fatalf("events = %+v; want exactly one Done carrying errStreamStalled", got)
	}
}

// TestErrorDetail: JSON error envelopes surface only their message field
// (providers may echo request details in the raw payload); anything else
// is a bounded one-line snippet.
func TestErrorDetail(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"error":{"message":"rate limited","type":"rate_limit"},"request_id":"secret-req"}`, "rate limited"},
		{`{"error":"bad thing"}`, "bad thing"},
		{`{"error":{"type":"x"}}`, "(json error body without message)"},
		{"boom\n  more", "boom more"},
		{"", "(empty body)"},
		{strings.Repeat("x", 300), strings.Repeat("x", maxErrorDetailBytes) + "…"},
	}
	for _, tc := range cases {
		if got := errorDetail([]byte(tc.body)); got != tc.want {
			t.Errorf("errorDetail(%q) = %q; want %q", tc.body, got, tc.want)
		}
	}
}
