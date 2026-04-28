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
		// chatgpt_account_id (direct claim)
		{"direct claim", "header.eyJjaGF0Z3B0X2FjY291bnRfaWQiOiJhY2N0LTEyMyJ9.sig", "acct-123"},
		// https://api.openai.com/auth.chatgpt_account_id (URI-form claim)
		{"uri claim", "header.eyJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGguY2hhdGdwdF9hY2NvdW50X2lkIjogImFjY3QtNDU2In0.sig", "acct-456"},
		// organizations[0].id (fallback)
		{"orgs claim", "header.eyJvcmdhbml6YXRpb25zIjogW3siaWQiOiAib3JnLTc4OSJ9XX0.sig", "org-789"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractAccountIDFromJWT(tt.token); got != tt.want {
				t.Errorf("extractAccountIDFromJWT() = %q, want %q", got, tt.want)
			}
		})
	}
}
