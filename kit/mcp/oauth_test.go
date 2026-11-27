package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

func TestCanonical(t *testing.T) {
	for in, want := range map[string]string{
		"HTTPS://Example.COM:443/mcp/":   "https://example.com/mcp",
		"http://127.0.0.1:8080/mcp#x":    "http://127.0.0.1:8080/mcp",
		"https://example.com":            "https://example.com",
		"https://example.com/t%2Fa/mcp/": "https://example.com/t%2Fa/mcp",
		"https://example.com/a%2F":       "https://example.com/a%2F",
	} {
		if got := canonical(in); got != want {
			t.Errorf("canonical(%q) = %q, want %q", in, got, want)
		}
	}
	// An escaped slash names another server: its sign-in must not be shared.
	for _, pair := range [][2]string{
		{"https://example.com/t%2Fa/mcp", "https://example.com/t/a/mcp"},
		{"https://example.com/t%2Fa/mcp/", "https://example.com/t/a/mcp"},
		{"https://example.com/a%2F", "https://example.com/a"},
	} {
		if canonical(pair[0]) == canonical(pair[1]) {
			t.Errorf("%s and %s share a key", pair[0], pair[1])
		}
	}
}

// TestTokenStore_ConcurrentWriters: stores opened on one file, as two
// nib processes would, keep every entry.
func TestTokenStore_ConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			store := OpenTokenStore(path) // a store of its own, like another process
			if err := store.update(fmt.Sprintf("https://s%d.example", i), &grant{Token: &oauth2.Token{AccessToken: "t"}}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	f, err := OpenTokenStore(path).read()
	if err != nil || len(f.Servers) != 16 {
		t.Errorf("entries = %d, %v; want all 16", len(f.Servers), err)
	}
}

// TestTokenStore: entries are kept by canonical address, in a file only
// the person can read, and two writes keep each other's entries.
func TestTokenStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nib", "mcp-auth.json")
	store := OpenTokenStore(path)
	if g, err := store.load("https://a.example/mcp"); g != nil || err != nil {
		t.Fatalf("empty store: %v, %v", g, err)
	}
	for _, endpoint := range []string{"https://A.example/mcp/", "https://b.example/mcp"} {
		if err := store.update(endpoint, &grant{ClientID: endpoint, Token: &oauth2.Token{AccessToken: "t"}}); err != nil {
			t.Fatal(err)
		}
	}
	g, err := store.load("https://a.example/mcp")
	if err != nil || g == nil || g.ClientID != "https://A.example/mcp/" {
		t.Errorf("load = %+v, %v; want the entry kept under its canonical address", g, err)
	}
	if other, _ := store.load("https://b.example/mcp"); other == nil {
		t.Error("the second write dropped the first entry's neighbour")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	if err := store.update("https://a.example/mcp", nil); err != nil {
		t.Fatal(err)
	}
	if g, _ := store.load("https://a.example/mcp"); g != nil {
		t.Error("a forgotten sign-in is still there")
	}
}

// tokenEndpoint is an authorization server's token endpoint that
// records each form it is sent.
type tokenEndpoint struct {
	mu    sync.Mutex
	forms []url.Values
	reply func(form url.Values) (int, string)
}

func (e *tokenEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	e.mu.Lock()
	e.forms = append(e.forms, r.PostForm)
	e.mu.Unlock()
	status, body := e.reply(r.PostForm)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := fmt.Fprint(w, body); err != nil {
		panic(err) // a test server that cannot write has no one to tell
	}
}

// TestGrantSource: an expired token is refreshed with the resource it
// was granted for, and the new one is kept. A refresh the server refuses
// asks for a sign-in.
func TestGrantSource(t *testing.T) {
	endpoint := &tokenEndpoint{reply: func(form url.Values) (int, string) {
		if form.Get("refresh_token") == "dead" {
			return http.StatusBadRequest, `{"error":"invalid_grant"}`
		}
		return http.StatusOK, `{"access_token":"fresh","token_type":"Bearer","expires_in":3600}`
	}}
	as := httptest.NewServer(endpoint)
	defer as.Close()
	store := OpenTokenStore(filepath.Join(t.TempDir(), "auth.json"))
	expired := grant{ClientID: "nib-client", TokenURL: as.URL, AuthStyle: oauth2.AuthStyleInParams, Resource: "https://mcp.example",
		Token: &oauth2.Token{AccessToken: "old", RefreshToken: "keep", Expiry: time.Now().Add(-time.Minute)}}
	src := newGrantSource("s", "https://mcp.example/mcp", store, expired)
	tok, err := src.Token()
	if err != nil || tok.AccessToken != "fresh" || tok.RefreshToken != "keep" {
		t.Fatalf("Token = %+v, %v; want a fresh token keeping the refresh token", tok, err)
	}
	form := endpoint.forms[0]
	if form.Get("grant_type") != "refresh_token" || form.Get("resource") != "https://mcp.example" || form.Get("client_id") != "nib-client" {
		t.Errorf("refresh form = %v; want the grant type, resource and client", form)
	}
	if kept, _ := store.load("https://mcp.example/mcp"); kept == nil || kept.Token.AccessToken != "fresh" {
		t.Errorf("kept = %+v; want the refreshed token stored", kept)
	}
	if again, err := src.Token(); err != nil || again.AccessToken != "fresh" || len(endpoint.forms) != 1 {
		t.Errorf("a valid token must be reused without a refresh: %v, %d refreshes", err, len(endpoint.forms))
	}

	dead := expired
	dead.Token = &oauth2.Token{AccessToken: "old", RefreshToken: "dead", Expiry: time.Now().Add(-time.Minute)}
	if _, err := newGrantSource("s", "x", store, dead).Token(); !isLoginRequired(err) {
		t.Errorf("refused refresh = %v, want a sign-in required", err)
	}
	none := expired
	none.Token = &oauth2.Token{AccessToken: "old", Expiry: time.Now().Add(-time.Minute)}
	if _, err := newGrantSource("s", "x", store, none).Token(); !isLoginRequired(err) {
		t.Errorf("no refresh token = %v, want a sign-in required", err)
	}
}

// TestGrantSource_SaveIsRetried: a refreshed token that could not be
// kept is kept on the next call, without another refresh.
func TestGrantSource_SaveIsRetried(t *testing.T) {
	endpoint := &tokenEndpoint{reply: func(url.Values) (int, string) {
		return http.StatusOK, `{"access_token":"fresh","token_type":"Bearer","refresh_token":"rotated","expires_in":3600}`
	}}
	as := httptest.NewServer(endpoint)
	defer as.Close()
	dir := t.TempDir()
	store := OpenTokenStore(dir) // a directory: every write fails
	g := grant{ClientID: "c", TokenURL: as.URL, AuthStyle: oauth2.AuthStyleInParams,
		Token: &oauth2.Token{AccessToken: "old", RefreshToken: "r", Expiry: time.Now().Add(-time.Minute)}}
	src := newGrantSource("s", "https://mcp.example", store, g)
	if tok, err := src.Token(); err != nil || tok.AccessToken != "fresh" {
		t.Fatalf("Token = %+v, %v; want the refreshed token even when it cannot be kept", tok, err)
	}
	store.path = filepath.Join(dir, "auth.json") // the disk is back
	if _, err := src.Token(); err != nil {
		t.Fatal(err)
	}
	kept, err := store.load("https://mcp.example")
	if err != nil || kept == nil || kept.Token.RefreshToken != "rotated" || len(endpoint.forms) != 1 {
		t.Errorf("kept = %+v, %v after %d refreshes; want the rotated token saved without a second refresh", kept, err, len(endpoint.forms))
	}
}

// TestGrantSource_RefreshStaysOnTheTokenEndpoint: the refresh token is
// not replayed to wherever the token endpoint redirects.
func TestGrantSource_RefreshStaysOnTheTokenEndpoint(t *testing.T) {
	var elsewhere atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Store(true) }))
	defer other.Close()
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/token", http.StatusTemporaryRedirect)
	}))
	defer as.Close()
	g := grant{ClientID: "c", TokenURL: as.URL, AuthStyle: oauth2.AuthStyleInParams,
		Token: &oauth2.Token{AccessToken: "old", RefreshToken: "secret", Expiry: time.Now().Add(-time.Minute)}}
	if _, err := newGrantSource("s", "x", OpenTokenStore(filepath.Join(t.TempDir(), "a.json")), g).Token(); err == nil {
		t.Error("a refresh redirected to another origin must fail")
	}
	if elsewhere.Load() {
		t.Error("the refresh token reached the other origin")
	}
	plain := g
	plain.TokenURL = "http://as.example/token"
	if _, err := newGrantSource("s", "x", OpenTokenStore(filepath.Join(t.TempDir(), "b.json")), plain).Token(); err == nil {
		t.Error("a refresh over plain http to another machine must be refused")
	}
}

// TestLogin_FailureKeepsTheSignIn: a sign-in that does not complete
// leaves the earlier one in place.
func TestLogin_FailureKeepsTheSignIn(t *testing.T) {
	as, _ := fakeAS(t, "good")
	srv := guarded(t, "good", as.URL)
	server := Server{Name: "remote", URL: srv.URL + "/mcp", Origin: OriginUser}
	store := OpenTokenStore(filepath.Join(t.TempDir(), "auth.json"))
	if err := store.update(server.URL, &grant{Token: &oauth2.Token{AccessToken: "earlier"}}); err != nil {
		t.Fatal(err)
	}
	opts := LoginOptions{Open: func(string) error { return errors.New("no browser") }, Timeout: 200 * time.Millisecond}
	if err := Login(context.Background(), server, store, opts); err == nil {
		t.Fatal("a sign-in nobody completed must fail")
	}
	if g, err := store.load(server.URL); err != nil || g == nil || g.Token.AccessToken != "earlier" {
		t.Errorf("after the failed sign-in: %+v, %v; want the earlier one kept", g, err)
	}
}

func isLoginRequired(err error) bool {
	_, ok := errors.AsType[*LoginRequiredError](err)
	return ok
}

// guarded serves an MCP server that answers only requests carrying the
// token, and points a client without one at its authorization server.
func guarded(t *testing.T, token string, as string) *httptest.Server {
	t.Helper()
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return testServer() }, nil)
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"resource": srv.URL + "/mcp", "authorization_servers": []string{as}}) //nolint:errcheck // test server
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+srv.URL+`/.well-known/oauth-protected-resource/mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestSession_HTTPNeedsSignIn: a session cannot sign in, so a server
// that asks fails with a clear error; with a stored sign-in it starts.
func TestSession_HTTPNeedsSignIn(t *testing.T) {
	srv := guarded(t, "good", "https://as.invalid")
	server := Server{Name: "remote", URL: srv.URL + "/mcp", Origin: OriginUser}
	store := OpenTokenStore(filepath.Join(t.TempDir(), "auth.json"))

	s := Connect(context.Background(), []Server{server}, ConnectOptions{Tokens: store})
	t.Cleanup(func() { _ = s.Close() }) // test cleanup
	if len(s.Failed) != 1 || !isLoginRequired(s.Failed[0].Err) {
		t.Fatalf("failed = %v, want the sign-in required", s.Failed)
	}

	if err := store.update(server.URL, &grant{Token: &oauth2.Token{AccessToken: "good"}}); err != nil {
		t.Fatal(err)
	}
	s2 := Connect(context.Background(), []Server{server}, ConnectOptions{Tokens: store})
	t.Cleanup(func() { _ = s2.Close() }) // test cleanup
	if len(s2.Failed) != 0 {
		t.Fatalf("failed = %v, want the stored sign-in used", s2.Failed)
	}
	if names := definitionNames(s2); names != "mcp__remote__echo,list_mcp_resources,read_mcp_resource" {
		t.Errorf("tools = %s", names)
	}
}

// fakeAS is an authorization server for one sign-in: metadata, client
// registration, an authorization endpoint that redirects at once, and
// a token endpoint.
func fakeAS(t *testing.T, token string) (*httptest.Server, *tokenEndpoint) {
	t.Helper()
	var srv *httptest.Server
	tokens := &tokenEndpoint{reply: func(url.Values) (int, string) {
		return http.StatusOK, `{"access_token":"` + token + `","token_type":"Bearer","refresh_token":"r","expires_in":3600}`
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck // test server
			"issuer": srv.URL, "authorization_endpoint": srv.URL + "/authorize", "token_endpoint": srv.URL + "/token",
			"registration_endpoint": srv.URL + "/register", "code_challenge_methods_supported": []string{"S256"},
			"response_types_supported": []string{"code"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var meta map[string]any
		json.NewDecoder(r.Body).Decode(&meta) //nolint:errcheck // test server
		meta["client_id"] = "registered"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(meta) //nolint:errcheck // test server
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		back := q.Get("redirect_uri") + "?code=c&state=" + url.QueryEscape(q.Get("state"))
		http.Redirect(w, r, back, http.StatusFound)
	})
	mux.Handle("/token", tokens)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, tokens
}

// TestLogin: the browser flow runs once, the sign-in is kept with the
// resource it was granted for, and a session then uses it.
func TestLogin(t *testing.T) {
	as, tokens := fakeAS(t, "good")
	srv := guarded(t, "good", as.URL)
	server := Server{Name: "remote", URL: srv.URL + "/mcp", Origin: OriginUser}
	store := OpenTokenStore(filepath.Join(t.TempDir(), "auth.json"))
	var shown []string
	opts := LoginOptions{
		Open: func(address string) error {
			resp, err := http.Get(address) // the "browser" follows the redirect to the callback
			if err == nil {
				err = resp.Body.Close()
			}
			return err
		},
		Notify:  func(msg string) { shown = append(shown, msg) },
		Timeout: 10 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := Login(ctx, server, store, opts); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if len(shown) == 0 || !strings.Contains(shown[0], as.URL+"/authorize") {
		t.Errorf("shown %v, want the sign-in address", shown)
	}
	exchange := tokens.forms[0]
	if exchange.Get("resource") != srv.URL+"/mcp" || exchange.Get("code_verifier") == "" {
		t.Errorf("token request = %v, want the resource and a PKCE verifier", exchange)
	}
	g, err := store.load(server.URL)
	if err != nil || g == nil || g.Token.AccessToken != "good" || g.Resource != srv.URL+"/mcp" || g.ClientID != "registered" {
		t.Fatalf("kept = %+v, %v", g, err)
	}
	s := Connect(ctx, []Server{server}, ConnectOptions{Tokens: store})
	t.Cleanup(func() { _ = s.Close() }) // test cleanup
	if len(s.Failed) != 0 {
		t.Errorf("a session after Login failed: %v", s.Failed)
	}
}

// TestLogin_NotAsked: a server that never asks for a sign-in says so.
func TestLogin_NotAsked(t *testing.T) {
	srv := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return testServer() }, nil))
	defer srv.Close()
	store := OpenTokenStore(filepath.Join(t.TempDir(), "auth.json"))
	err := Login(context.Background(), Server{Name: "open", URL: srv.URL}, store, LoginOptions{})
	if !errors.Is(err, ErrNoLoginNeeded) {
		t.Errorf("Login = %v, want ErrNoLoginNeeded", err)
	}
}

func definitionNames(s *Session) string {
	var names []string
	for _, tool := range s.Tools() {
		names = append(names, tool.Definition().Function.Name)
	}
	return strings.Join(names, ",")
}

// TestGrantSource_KeepsARotatedRefreshToken: a refresh that keeps the
// access token but rotates the refresh token is still saved.
func TestGrantSource_KeepsARotatedRefreshToken(t *testing.T) {
	endpoint := &tokenEndpoint{reply: func(url.Values) (int, string) {
		return http.StatusOK, `{"access_token":"same","token_type":"Bearer","refresh_token":"rotated","expires_in":3600}`
	}}
	as := httptest.NewServer(endpoint)
	defer as.Close()
	store := OpenTokenStore(filepath.Join(t.TempDir(), "auth.json"))
	g := grant{ClientID: "c", TokenURL: as.URL, AuthStyle: oauth2.AuthStyleInParams,
		Token: &oauth2.Token{AccessToken: "same", RefreshToken: "first", Expiry: time.Now().Add(-time.Minute)}}
	if _, err := newGrantSource("s", "https://mcp.example", store, g).Token(); err != nil {
		t.Fatal(err)
	}
	if kept, err := store.load("https://mcp.example"); err != nil || kept == nil || kept.Token.RefreshToken != "rotated" {
		t.Errorf("kept = %+v, %v; want the rotated refresh token", kept, err)
	}
}

// TestSignInClient: every sign-in hop stays on https, and a request with
// a body (the code exchange) is not sent on to another origin.
func TestSignInClient(t *testing.T) {
	check := signInClient().CheckRedirect
	hop := func(method, from, to string) error {
		prev, err := http.NewRequest(method, from, nil)
		if err != nil {
			t.Fatal(err)
		}
		next, err := http.NewRequest(method, to, nil)
		if err != nil {
			t.Fatal(err)
		}
		return check(next, []*http.Request{prev})
	}
	for _, c := range []struct {
		method, from, to string
		ok               bool
	}{
		{http.MethodPost, "https://as.example/token", "http://as.example/token", false},
		{http.MethodPost, "https://as.example/token", "https://other.example/token", false},
		{http.MethodPost, "https://as.example/token", "https://as.example/v2/token", true},
		{http.MethodGet, "https://mcp.example/.well-known/x", "https://meta.example/x", true},
		{http.MethodGet, "https://mcp.example/.well-known/x", "http://meta.example/x", false},
		{http.MethodGet, "http://127.0.0.1:8080/a", "http://localhost:8080/b", true},
	} {
		if err := hop(c.method, c.from, c.to); (err == nil) != c.ok {
			t.Errorf("%s %s -> %s: %v, want allowed=%v", c.method, c.from, c.to, err, c.ok)
		}
	}
}
