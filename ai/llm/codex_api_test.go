package llm

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// fakeInvalidatableAuth records whether Invalidate was called, so tests
// can assert the 401 self-heal fired. It satisfies both Auth and
// CredentialInvalidator.
type fakeInvalidatableAuth struct{ invalidated bool }

func (f *fakeInvalidatableAuth) Authenticate(context.Context, *http.Request) error { return nil }
func (f *fakeInvalidatableAuth) Invalidate()                                       { f.invalidated = true }

func TestCodexAPI_AuthFailureInvalidatesAndReturnsTypedError(t *testing.T) {
	auth := &fakeInvalidatableAuth{}
	c := NewCodexAPI("gpt-5-codex", auth)

	body := []byte(`{"error":{"message":"Encountered invalidated oauth token","code":"token_revoked"},"status":401}`)
	err := c.authFailure(http.StatusUnauthorized, body)

	if !auth.invalidated {
		t.Error("authFailure did not call Invalidate on the credential")
	}
	var ae *AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("authFailure returned %T, want *AuthError", err)
	}
	if ae.Code != "token_revoked" {
		t.Errorf("AuthError.Code = %q, want token_revoked", ae.Code)
	}
}

// staticAuth satisfies Auth but NOT CredentialInvalidator — a 401 against
// API-key auth must not panic and must still return a typed AuthError.
type staticAuth struct{}

func (staticAuth) Authenticate(context.Context, *http.Request) error { return nil }

func TestCodexAPI_AuthFailureNoInvalidatorStillTypesError(t *testing.T) {
	c := NewCodexAPI("gpt-5-codex", staticAuth{})

	err := c.authFailure(http.StatusUnauthorized, nil)

	var ae *AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("authFailure returned %T, want *AuthError", err)
	}
	if ae.StatusCode != http.StatusUnauthorized {
		t.Errorf("AuthError.StatusCode = %d, want 401", ae.StatusCode)
	}
}
