package llm

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestParseAuthError_TokenRevoked(t *testing.T) {
	body := []byte(`{"error":{"message":"Encountered invalidated oauth token for user, failing request","type":null,"code":"token_revoked","param":null},"status":401}`)
	e := parseAuthError("chatgpt", 401, body)

	if e.Provider != "chatgpt" {
		t.Errorf("Provider = %q, want chatgpt", e.Provider)
	}
	if e.StatusCode != 401 {
		t.Errorf("StatusCode = %d, want 401", e.StatusCode)
	}
	if e.Code != "token_revoked" {
		t.Errorf("Code = %q, want token_revoked", e.Code)
	}
	if !strings.Contains(e.Message, "invalidated oauth token") {
		t.Errorf("Message = %q, want it to carry the body detail", e.Message)
	}
}

func TestParseAuthError_MalformedBodyDegradesGracefully(t *testing.T) {
	// A non-JSON body must not panic or error — Code/Message stay empty
	// and the status code remains the only detail.
	e := parseAuthError("chatgpt", 401, []byte("Service Unavailable"))
	if e.Code != "" || e.Message != "" {
		t.Errorf("expected empty Code/Message for non-JSON body, got code=%q message=%q", e.Code, e.Message)
	}
	if e.StatusCode != 401 {
		t.Errorf("StatusCode = %d, want 401", e.StatusCode)
	}
}

func TestAuthError_ErrorIsActionable(t *testing.T) {
	cases := []struct {
		name string
		err  *AuthError
		want []string // substrings that must appear
	}{
		{
			name: "code and message",
			err:  &AuthError{Provider: "chatgpt", StatusCode: 401, Code: "token_revoked", Message: "bad token"},
			want: []string{"chatgpt", "token_revoked", "bad token", "re-authenticate"},
		},
		{
			name: "code only",
			err:  &AuthError{Provider: "chatgpt", StatusCode: 401, Code: "token_revoked"},
			want: []string{"chatgpt", "token_revoked", "re-authenticate"},
		},
		{
			name: "neither code nor message",
			err:  &AuthError{Provider: "chatgpt", StatusCode: 401},
			want: []string{"chatgpt", "401", "re-authenticate"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.err.Error()
			for _, sub := range tc.want {
				if !strings.Contains(msg, sub) {
					t.Errorf("Error() = %q, want it to contain %q", msg, sub)
				}
			}
		})
	}
}

func TestAuthError_ErrorsAsThroughWrapping(t *testing.T) {
	// The provider/proxy layers wrap Stream errors (e.g.
	// "provider.Stream: %w"), so callers must still recover the typed
	// error via errors.As to drive token invalidation / actionable UI.
	chain := fmt.Errorf("provider.Stream: %w", &AuthError{Provider: "chatgpt", StatusCode: 401, Code: "token_revoked"})
	var ae *AuthError
	if !errors.As(chain, &ae) {
		t.Fatal("errors.As failed to recover *AuthError through wrapping")
	}
	if ae.Code != "token_revoked" {
		t.Errorf("recovered Code = %q, want token_revoked", ae.Code)
	}
}
