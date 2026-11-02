package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

// tokenEndpoint serves a fixed status and body for every token request.
func tokenEndpoint(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body)) // test server; client sees a short body on failure
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPostTokenForm_ClassifiesRejection(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		body         string
		wantMsg      string
		wantRejected bool
	}{
		{"invalid_grant", http.StatusBadRequest, `{"error":"invalid_grant","error_description":"Refresh token expired"}`, "refresh: HTTP 400: Refresh token expired", true},
		{"unauthorized", http.StatusUnauthorized, `{"error":"refresh_token_reused"}`, "refresh: HTTP 401: refresh_token_reused", true},
		{"bad request is not a rejected grant", http.StatusBadRequest, `{"error":"invalid_request"}`, "refresh: HTTP 400: invalid_request", false},
		{"server error", http.StatusInternalServerError, `oops`, "refresh: HTTP 500", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := tokenEndpoint(t, tc.status, tc.body)
			_, err := postTokenForm(context.Background(), srv.URL, "refresh", url.Values{})
			te, ok := errors.AsType[*tokenEndpointError](err)
			if !ok {
				t.Fatalf("err = %v, want *tokenEndpointError", err)
			}
			if err.Error() != tc.wantMsg {
				t.Errorf("message = %q, want %q", err.Error(), tc.wantMsg)
			}
			if te.grantRejected() != tc.wantRejected {
				t.Errorf("grantRejected = %v, want %v", te.grantRejected(), tc.wantRejected)
			}
		})
	}
}

// expiredOpenAIToken is an expired access token that still carries a
// refresh token.
func expiredOpenAIToken() *Token {
	return &Token{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour)}
}

func TestOpenAITokenSource_RefreshFailure(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantToken bool
	}{
		{"rejected grant drops token", http.StatusBadRequest, `{"error":"invalid_grant"}`, false},
		{"transient failure keeps token", http.StatusBadGateway, ``, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := seedOpenAIStore(t, expiredOpenAIToken())
			src := NewOpenAITokenSource(store)
			src.tokenURL = tokenEndpoint(t, tc.status, tc.body).URL

			if _, err := src.Token(context.Background()); err == nil {
				t.Fatal("Token: want error from failed refresh")
			}
			if got := store.Get(ProviderOpenAI) != nil; got != tc.wantToken {
				t.Errorf("token kept after failure = %v, want %v", got, tc.wantToken)
			}
		})
	}
}

func TestOpenAITokenSource_RefreshesExpiredToken(t *testing.T) {
	store := seedOpenAIStore(t, expiredOpenAIToken())
	src := NewOpenAITokenSource(store)
	src.tokenURL = tokenEndpoint(t, http.StatusOK, `{"access_token":"new","expires_in":3600}`).URL

	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.AccessToken != "new" || tok.RefreshToken != "refresh" {
		t.Errorf("token = %+v, want new access token and the original refresh token kept", tok)
	}
	if !store.HasToken(ProviderOpenAI) {
		t.Error("refreshed token not persisted")
	}
}

// TestOpenAITokenSource_RejectedRefreshKeepsConcurrentWinner covers two
// sources refreshing the same token: the loser's refresh is rejected as
// reused, but the winner's freshly stored token must survive and be used.
func TestOpenAITokenSource_RejectedRefreshKeepsConcurrentWinner(t *testing.T) {
	store := seedOpenAIStore(t, expiredOpenAIToken())
	winner := &Token{AccessToken: "won", RefreshToken: "rotated", ExpiresAt: time.Now().Add(time.Hour)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := store.Put(ProviderOpenAI, winner); err != nil { // the concurrent winner lands first
			t.Errorf("Put winner: %v", err)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"refresh_token_reused"}`)) // test server; client sees a short body on failure
	}))
	t.Cleanup(srv.Close)
	src := NewOpenAITokenSource(store)
	src.tokenURL = srv.URL

	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v; want the concurrent winner's token", err)
	}
	if tok.AccessToken != "won" {
		t.Errorf("token = %q, want winner's", tok.AccessToken)
	}
	if got := store.Get(ProviderOpenAI); got == nil || got.RefreshToken != "rotated" {
		t.Errorf("stored token = %+v; winner's token was deleted", got)
	}
}

func TestStoreDeleteIf(t *testing.T) {
	store := seedOpenAIStore(t, &Token{AccessToken: "a", RefreshToken: "r1"})
	if err := store.DeleteIf(ProviderOpenAI, func(cur Token) bool { return cur.RefreshToken == "other" }); err != nil {
		t.Fatalf("DeleteIf mismatch: %v", err)
	}
	if store.Get(ProviderOpenAI) == nil {
		t.Fatal("DeleteIf removed a token its predicate rejected")
	}
	if err := store.DeleteIf(ProviderOpenAI, func(cur Token) bool { return cur.RefreshToken == "r1" }); err != nil {
		t.Fatalf("DeleteIf match: %v", err)
	}
	if store.Get(ProviderOpenAI) != nil {
		t.Error("DeleteIf kept a token its predicate accepted")
	}
	if err := store.DeleteIf(ProviderCopilot, func(Token) bool { return true }); err != nil {
		t.Errorf("DeleteIf on missing token: %v", err)
	}
}

// TestOpenAITokenSource_UsesTokenAnotherProcessRefreshed: a source whose
// cache is expired must pick up a token another process already rotated
// instead of spending its stale refresh token.
func TestOpenAITokenSource_UsesTokenAnotherProcessRefreshed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	stale := openStore(t, path)
	if err := stale.Put(ProviderOpenAI, expiredOpenAIToken()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	other := openStore(t, path)
	if err := other.Put(ProviderOpenAI, &Token{AccessToken: "fresh", RefreshToken: "rotated", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("other process refresh: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("refresh endpoint called; the rotated token on disk should have been used")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	src := NewOpenAITokenSource(stale)
	src.tokenURL = srv.URL

	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.AccessToken != "fresh" {
		t.Errorf("token = %q, want the other process's", tok.AccessToken)
	}
}
