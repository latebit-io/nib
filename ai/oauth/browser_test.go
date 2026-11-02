package oauth

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStartCallbackServer_BusyPortFailsWithoutFallback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }() // test cleanup
	port := ln.Addr().(*net.TCPAddr).Port

	cb, err := startCallbackServer("127.0.0.1", port, "state", "/auth/callback")
	if err == nil {
		cb.close()
		t.Fatalf("want error for busy port %d; a fallback port breaks exact redirect_uri matching (got %s)", port, cb.redirectURI)
	}
}

func TestCallbackServer_PageReflectsExchangeResult(t *testing.T) {
	tests := []struct {
		name        string
		exchangeErr error
		want        string
	}{
		{"success", nil, "Authentication successful"},
		{"failure", errors.New("token exchange: HTTP 400: bad code"), "Authentication failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cb, err := startCallbackServer("127.0.0.1", 0, "st", "/auth/callback")
			if err != nil {
				t.Fatalf("startCallbackServer: %v", err)
			}
			defer cb.close()

			type page struct {
				body string
				err  error
			}
			got := make(chan page, 1)
			go func() {
				resp, err := http.Get(cb.redirectURI + "?state=st&code=c1")
				if err != nil {
					got <- page{err: err}
					return
				}
				defer func() { _ = resp.Body.Close() }() // test cleanup
				b, err := io.ReadAll(resp.Body)
				got <- page{body: string(b), err: err}
			}()

			select {
			case code := <-cb.codeCh:
				if code != "c1" {
					t.Fatalf("code = %q, want c1", code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("callback never delivered the code")
			}
			select {
			case p := <-got:
				t.Fatalf("page rendered before the exchange finished: %q", p.body)
			case <-time.After(50 * time.Millisecond):
			}

			cb.resultCh <- tc.exchangeErr
			select {
			case p := <-got:
				if p.err != nil {
					t.Fatalf("GET callback: %v", p.err)
				}
				if !strings.Contains(p.body, tc.want) {
					t.Errorf("page = %q, want it to contain %q", p.body, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("page never rendered after the exchange result")
			}
		})
	}
}

// A handler that delivered its code must not hold close for the full
// shutdown timeout when the flow ends without exchanging it.
func TestCallbackServer_CloseReleasesWaitingHandler(t *testing.T) {
	cb, err := startCallbackServer("127.0.0.1", 0, "st", "/auth/callback")
	if err != nil {
		t.Fatalf("startCallbackServer: %v", err)
	}
	body := make(chan string, 1)
	go func() {
		resp, err := http.Get(cb.redirectURI + "?state=st&code=c1")
		if err != nil {
			body <- err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }() // test cleanup
		b, _ := io.ReadAll(resp.Body)            // empty body fails the assertion below
		body <- string(b)
	}()
	select {
	case <-cb.codeCh:
	case <-time.After(5 * time.Second):
		t.Fatal("callback never delivered the code")
	}

	start := time.Now()
	cb.close()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("close took %s; a waiting handler stalled shutdown", elapsed)
	}
	if got := <-body; !strings.Contains(got, "Authentication failed") {
		t.Errorf("page = %q, want failure page", got)
	}
}

func TestBrowserFlow_CanceledContextOpensNothing(t *testing.T) {
	orig := openBrowser
	t.Cleanup(func() { openBrowser = orig })
	openBrowser = func(string) error {
		t.Error("canceled flow opened the browser")
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := browserFlow(ctx, browserFlowConfig{AuthURL: "http://127.0.0.1/unused", RedirectHost: "127.0.0.1"}, &FlowCallbacks{
		OnBrowserOpen: func(string) { t.Error("canceled flow announced a browser URL") },
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("browserFlow err = %v, want context.Canceled", err)
	}
}
