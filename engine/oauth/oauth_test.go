package oauth

import (
	"testing"
	"time"
)

func TestTokenValid(t *testing.T) {
	tests := []struct {
		name  string
		token *Token
		want  bool
	}{
		{"nil token", nil, false},
		{"empty access token", &Token{}, false},
		{"valid no expiry", &Token{AccessToken: "abc"}, true},
		{"valid future expiry", &Token{AccessToken: "abc", ExpiresAt: time.Now().Add(time.Hour)}, true},
		{"expired", &Token{AccessToken: "abc", ExpiresAt: time.Now().Add(-time.Hour)}, false},
		{"about to expire (within 30s buffer)", &Token{AccessToken: "abc", ExpiresAt: time.Now().Add(15 * time.Second)}, false},
		{"just barely valid", &Token{AccessToken: "abc", ExpiresAt: time.Now().Add(45 * time.Second)}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.token.Valid(); got != tt.want {
				t.Errorf("Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractAccountIDFromJWT(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  string
	}{
		{"not a JWT", "not-a-jwt", ""},
		{"empty", "", ""},
		{"invalid base64", "header.!!!invalid!!!.sig", ""},
		// A JWT with {"chatgpt_account_id":"acct-123"} payload:
		// base64url("eyJjaGF0Z3B0X2FjY291bnRfaWQiOiJhY2N0LTEyMyJ9")
		{"direct claim", "header.eyJjaGF0Z3B0X2FjY291bnRfaWQiOiJhY2N0LTEyMyJ9.sig", "acct-123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractAccountIDFromJWT(tt.token); got != tt.want {
				t.Errorf("extractAccountIDFromJWT() = %q, want %q", got, tt.want)
			}
		})
	}
}
