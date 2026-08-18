package llm

import (
	"bufio"
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

// TestCodexSSE_ScannerErrorEmitsTerminalErr: a read failure with ctx live
// must end the stream with a terminal Err, never a clean Done — the agent
// would otherwise treat possibly partial tool calls as a completion — and
// the incomplete event pending at the failure is discarded.
func TestCodexSSE_ScannerErrorEmitsTerminalErr(t *testing.T) {
	c := NewCodexAPI("m", staticAuth{}, "")
	body := errBody(
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"+
			"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"f\"}}\n",
		errors.New("connection reset"))
	ch := make(chan StreamEvent, 8)
	c.readCodexSSE(context.Background(), body, ch)
	close(ch)

	var tokens int
	var last StreamEvent
	for ev := range ch {
		if ev.Token != "" {
			tokens++
		}
		last = ev
	}
	if tokens != 1 {
		t.Errorf("tokens = %d; want 1 (delta before the failure)", tokens)
	}
	if !last.Done || last.Err == nil || !strings.Contains(last.Err.Error(), "connection reset") {
		t.Fatalf("last event = %+v; want Done with the read error", last)
	}
	if len(last.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %+v; the incomplete event after the failure must not dispatch", last.ToolCalls)
	}
}

// TestScanSSE_CleanEOFEmitsTerminal covers the shared loop's fallback: no
// stop from the dispatcher, body ends cleanly, terminal() is emitted once.
func TestScanSSE_CleanEOFEmitsTerminal(t *testing.T) {
	// Three events: typed multi-line data (joined by \n), an untyped one
	// (the type does not carry over), and a trailing one without a final
	// blank line (dispatched at clean EOF).
	body := io.NopCloser(strings.NewReader("event: ping\ndata: a1\ndata: a2\n\n: comment\ndata: b\n\ndata: c\n"))
	ch := make(chan StreamEvent, 4)
	var seen []string
	scanSSE(context.Background(), body, ch, "test", func(eventType, data string) bool {
		seen = append(seen, eventType+"/"+data)
		return false
	}, func() StreamEvent { return StreamEvent{Done: true} })
	close(ch)

	if got := strings.Join(seen, "|"); got != "ping/a1\na2|/b|/c" {
		t.Errorf("dispatched = %q; want %q", got, "ping/a1\na2|/b|/c")
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
// Close, after which it reports clean EOF — a connection that stalls
// mid-stream and then closes without error.
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

// TestScanSSE_StalledReadEmitsStalledNotTerminal: a Read that never
// returns trips the watchdog; the closed body's clean EOF must then
// surface errStreamStalled, never terminal() success. Events served
// before the stall still dispatch.
func TestScanSSE_StalledReadEmitsStalledNotTerminal(t *testing.T) {
	prev := sseIdleTimeout
	sseIdleTimeout = 20 * time.Millisecond
	t.Cleanup(func() { sseIdleTimeout = prev })

	body := newBlockingBody("data: a\n\n")
	ch := make(chan StreamEvent, 4)
	var seen []string
	scanSSE(context.Background(), body, ch, "test", func(_, data string) bool {
		seen = append(seen, data)
		return false
	}, func() StreamEvent { return StreamEvent{Done: true} })
	close(ch)

	if strings.Join(seen, ",") != "a" {
		t.Errorf("dispatched = %v; want [a]", seen)
	}
	var got []StreamEvent
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) != 1 || !got[0].Done || !errors.Is(got[0].Err, errStreamStalled) {
		t.Fatalf("events = %+v; want exactly one Done carrying errStreamStalled", got)
	}
}

// TestScanSSE_SlowConsumerIsNotAStall: idle time is measured across the
// blocking read only, so a dispatcher that takes longer than the idle
// timeout (downstream backpressure) does not trip the watchdog.
func TestScanSSE_SlowConsumerIsNotAStall(t *testing.T) {
	prev := sseIdleTimeout
	sseIdleTimeout = 20 * time.Millisecond
	t.Cleanup(func() { sseIdleTimeout = prev })

	body := io.NopCloser(strings.NewReader("data: a\n\ndata: b\n\n"))
	ch := make(chan StreamEvent, 4)
	var seen []string
	scanSSE(context.Background(), body, ch, "test", func(_, data string) bool {
		seen = append(seen, data)
		time.Sleep(3 * sseIdleTimeout)
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
	if len(got) != 1 || !got[0].Done || got[0].Err != nil {
		t.Fatalf("events = %+v; want exactly one clean Done", got)
	}
}

// TestScanSSE_OversizedLineSurfacesErr: a scanner failure that is neither
// a stall nor a cancellation reaches the consumer as a terminal Err.
func TestScanSSE_OversizedLineSurfacesErr(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: " + strings.Repeat("x", sseMaxLineBytes+1) + "\n\n"))
	ch := make(chan StreamEvent, 4)
	scanSSE(context.Background(), body, ch, "test", func(_, _ string) bool { return false },
		func() StreamEvent { return StreamEvent{Done: true} })
	close(ch)

	var got []StreamEvent
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) != 1 || !got[0].Done || !errors.Is(got[0].Err, bufio.ErrTooLong) {
		t.Fatalf("events = %+v; want exactly one Done carrying bufio.ErrTooLong", got)
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
