package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
	"unicode/utf8"
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

	last, tokens := terminalOf(t, collectStream(ch))
	if tokens != 1 {
		t.Errorf("tokens = %d; want 1 (delta before the failure)", tokens)
	}
	if last.Err == nil || !strings.Contains(last.Err.Error(), "connection reset") {
		t.Fatalf("last event = %+v; want Done with the read error", last)
	}
	if len(last.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %+v; the incomplete event after the failure must not dispatch", last.ToolCalls)
	}
}

// TestScanSSE_CleanEOFEmitsTerminal covers the shared loop's fallback: no
// stop from the dispatcher, body ends cleanly, terminal() is emitted once.
func TestScanSSE_CleanEOFEmitsTerminal(t *testing.T) {
	// Five events: typed multi-line data (joined by \n), an untyped one
	// (the type does not carry over), a value keeping its extra leading
	// and trailing spaces (only one leading space is stripped), a
	// colon-less "data" field (empty value, still an event), and a
	// trailing one without a final blank line (dispatched at clean EOF).
	// Comments, id, retry, and unknown fields are skipped.
	body := io.NopCloser(strings.NewReader(
		"event: ping\ndata: a1\ndata: a2\n\n: comment\nid: 7\ndata: b\n\ndata:  sp \n\ndata\n\ndatabase: nope\ndata: c\n"))
	ch := make(chan StreamEvent, 4)
	var seen []string
	scanSSE(context.Background(), body, ch, "test", func(eventType, data string) (bool, error) {
		seen = append(seen, eventType+"/"+data)
		return false, nil
	}, func() StreamEvent { return StreamEvent{Done: true} })
	close(ch)

	want := "ping/a1\na2|/b|/ sp |/|/c"
	if got := strings.Join(seen, "|"); got != want {
		t.Errorf("dispatched = %q; want %q", got, want)
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

// TestScanSSE_EndOfStreamPolicy: "[DONE]" ends the stream like clean EOF
// (later events never dispatch), a nil terminal turns that end into
// errStreamEnded, and an onEvent error becomes the provider-prefixed
// terminal Err.
func TestScanSSE_EndOfStreamPolicy(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name     string
		body     string
		terminal func() StreamEvent
		wantSeen string
		wantErr  error // nil: clean Done
	}{
		{"[DONE] emits terminal", "data: a\n\ndata: [DONE]\n\ndata: b\n\n", func() StreamEvent { return StreamEvent{Done: true} }, "a", nil},
		{"nil terminal fails at [DONE]", "data: a\n\ndata: [DONE]\n\n", nil, "a", errStreamEnded},
		{"nil terminal fails at EOF", "data: a\n\n", nil, "a", errStreamEnded},
		{"onEvent error", "data: fail\n\ndata: b\n\n", nil, "fail", boom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := make(chan StreamEvent, 4)
			var seen []string
			scanSSE(context.Background(), io.NopCloser(strings.NewReader(tc.body)), ch, "test", func(_, data string) (bool, error) {
				seen = append(seen, data)
				if data == "fail" {
					return false, boom
				}
				return false, nil
			}, tc.terminal)
			close(ch)

			if got := strings.Join(seen, ","); got != tc.wantSeen {
				t.Errorf("dispatched = %q; want %q", got, tc.wantSeen)
			}
			last, _ := terminalOf(t, collectStream(ch))
			if tc.wantErr == nil {
				if last.Err != nil {
					t.Fatalf("terminal Err = %v; want clean Done", last.Err)
				}
				return
			}
			if !errors.Is(last.Err, tc.wantErr) || !strings.HasPrefix(last.Err.Error(), "test: ") {
				t.Fatalf("terminal Err = %v; want %v prefixed with the provider", last.Err, tc.wantErr)
			}
		})
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
	scanSSE(context.Background(), body, ch, "test", func(_, data string) (bool, error) {
		seen = append(seen, data)
		return false, nil
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
	scanSSE(context.Background(), body, ch, "test", func(_, data string) (bool, error) {
		seen = append(seen, data)
		time.Sleep(3 * sseIdleTimeout)
		return false, nil
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
	scanSSE(context.Background(), body, ch, "test", func(_, _ string) (bool, error) { return false, nil },
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

// TestAPIErrorDetail: a streamed error renders as "code (param p): message",
// falling back to its type for the code and bounding every field.
func TestAPIErrorDetail(t *testing.T) {
	long := strings.Repeat("x", 300)
	cases := []struct {
		err  apiError
		want string
	}{
		{apiError{Type: "invalid_request_error", Code: "bad_schema", Param: "tools", Message: "no"}, "bad_schema (param tools): no"},
		{apiError{Type: "overloaded_error", Message: "Overloaded"}, "overloaded_error: Overloaded"},
		{apiError{Message: "line one\n  line two"}, "line one line two"},
		{apiError{}, "(no message)"},
		{apiError{Code: errorCode(long), Message: long}, long[:maxErrorDetailBytes] + "…: " + long[:maxErrorDetailBytes] + "…"},
	}
	for _, tc := range cases {
		if got := tc.err.detail(); got != tc.want {
			t.Errorf("detail(%+v) = %q; want %q", tc.err, got, tc.want)
		}
	}
}

// TestErrorCode_UnmarshalJSON: codes arrive as strings or numbers.
func TestErrorCode_UnmarshalJSON(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"code":"server_error"}`, "server_error"},
		{`{"code":502}`, "502"},
		{`{"code":null}`, ""},
		{`{}`, ""},
	}
	for _, tc := range cases {
		var e apiError
		if err := json.Unmarshal([]byte(tc.in), &e); err != nil {
			t.Errorf("Unmarshal(%s): %v", tc.in, err)
			continue
		}
		if string(e.Code) != tc.want {
			t.Errorf("Unmarshal(%s).Code = %q; want %q", tc.in, e.Code, tc.want)
		}
	}
	var e apiError
	if err := json.Unmarshal([]byte(`{"code":{"nested":1}}`), &e); err == nil {
		t.Error("object code decoded without error")
	}
}

// TestOneLine_CutsOnRuneBoundary: bounding multi-byte text must not split
// a rune and emit invalid UTF-8.
func TestOneLine_CutsOnRuneBoundary(t *testing.T) {
	got := oneLine(strings.Repeat("é", maxErrorDetailBytes)) // 2 bytes per rune; 255 is mid-rune
	if !utf8.ValidString(got) {
		t.Fatalf("oneLine produced invalid UTF-8: %q", got)
	}
	if body, ok := strings.CutSuffix(got, "…"); !ok || len(body) > maxErrorDetailBytes {
		t.Errorf("oneLine = %d bytes; want at most %d plus an ellipsis", len(got), maxErrorDetailBytes)
	}
}

// TestAgentAPI_StreamedErrorEndsStream: an OpenAI-compatible error chunk
// inside an HTTP-200 stream (OpenRouter streams numeric codes) ends the
// stream with a terminal Err, not a successful empty completion.
func TestAgentAPI_StreamedErrorEndsStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, // test server; write error irrelevant
			"data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\n"+
				"data: {\"error\":{\"code\":502,\"message\":\"Provider disconnected unexpectedly\"},\"choices\":[{\"delta\":{\"content\":\"\"},\"finish_reason\":\"error\"}]}\n\n"+
				"data: [DONE]\n\n")
	}))
	defer srv.Close()

	ch, err := NewAgentAPI(srv.URL+"/", "m", staticAuth{}, false, "").Stream(context.Background(), []Message{{Role: "user", Content: "x"}}, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	last, tokens := terminalOf(t, collectStream(ch))
	const want = "openai-compat: stream error: 502: Provider disconnected unexpectedly"
	if last.Err == nil || last.Err.Error() != want || tokens != 1 {
		t.Fatalf("terminal Err = %v after %d tokens; want %q after 1", last.Err, tokens, want)
	}
}

// TestAnthropicStream_ErrorEventDetail: a streamed error event surfaces its
// type and message; an unparseable one a bounded snippet, never the raw
// payload.
func TestAnthropicStream_ErrorEventDetail(t *testing.T) {
	cases := []struct{ data, want string }{
		{`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, "stream error: overloaded_error: Overloaded"},
		{"not json " + strings.Repeat("x", 300), "stream error: " + ("not json " + strings.Repeat("x", 300))[:maxErrorDetailBytes] + "…"},
	}
	for _, tc := range cases {
		_, err := newAnthropicState().dispatchEvent(context.Background(), "error", tc.data, nil)
		if err == nil || err.Error() != tc.want {
			t.Errorf("dispatchEvent err = %v; want %q", err, tc.want)
		}
	}
}
