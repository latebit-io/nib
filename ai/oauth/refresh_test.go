package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
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
