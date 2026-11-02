package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
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
	body := []byte(`{"error":{"message":"Encountered invalidated oauth token","code":"token_revoked"},"status":401}`)
	err := authFailure(codexProviderName, auth, http.StatusUnauthorized, body)

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
	err := authFailure(codexProviderName, staticAuth{}, http.StatusUnauthorized, nil)

	var ae *AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("authFailure returned %T, want *AuthError", err)
	}
	if ae.StatusCode != http.StatusUnauthorized {
		t.Errorf("AuthError.StatusCode = %d, want 401", ae.StatusCode)
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCodexAPI_ListModelsSkipsHiddenAndSendsClientVersion(t *testing.T) {
	c := NewCodexAPI("", staticAuth{}, "")
	var gotVersion string
	c.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotVersion = r.URL.Query().Get("client_version")
		body := `{"models":[
			{"slug":"gpt-6.1-sol","display_name":"GPT-6.1-Sol","visibility":"list"},
			{"slug":"gpt-reserve","display_name":"GPT-Reserve","visibility":"hide"},
			{"slug":"gpt-6-luna","visibility":"list"}]}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}

	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotVersion != codexClientVersion {
		t.Errorf("client_version = %q, want %q; the endpoint lists nothing without a current one", gotVersion, codexClientVersion)
	}
	want := []ModelInfo{{ID: "gpt-6.1-sol", Name: "GPT-6.1-Sol"}, {ID: "gpt-6-luna", Name: "gpt-6-luna"}}
	if !slices.Equal(models, want) {
		t.Errorf("models = %+v, want %+v", models, want)
	}
}
