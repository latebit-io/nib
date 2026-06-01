package oauth

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// newStoreWithOpenAIToken returns a store seeded with a valid (unexpired)
// OpenAI token, mirroring the revoked-but-locally-valid state that the
// self-heal path must clear.
func newStoreWithOpenAIToken(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Put(ProviderOpenAI, &Token{
		AccessToken:  "access",
		RefreshToken: "refresh",
		ExpiresAt:    time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !store.HasToken(ProviderOpenAI) {
		t.Fatal("precondition: seeded token should be valid")
	}
	return store
}

func TestOpenAITokenSource_InvalidateDeletesToken(t *testing.T) {
	store := newStoreWithOpenAIToken(t)
	src := NewOpenAITokenSource(store)

	if err := src.Invalidate(); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if store.HasToken(ProviderOpenAI) {
		t.Error("token still present after Invalidate; HasToken must report false so connect is re-offered")
	}
	if store.Get(ProviderOpenAI) != nil {
		t.Error("Get returned a token after Invalidate; want nil")
	}
}

func TestAuthenticator_InvalidateDelegatesToSource(t *testing.T) {
	store := newStoreWithOpenAIToken(t)
	auth := NewAuthenticator(NewOpenAITokenSource(store))

	auth.Invalidate()

	if store.HasToken(ProviderOpenAI) {
		t.Error("Authenticator.Invalidate did not clear the token through the source")
	}
}

// nonInvalidatableSource is a TokenSource without an Invalidate method, so
// it does NOT satisfy invalidatableSource.
type nonInvalidatableSource struct{}

func (nonInvalidatableSource) Token(context.Context) (*Token, error) {
	return &Token{AccessToken: "x"}, nil
}

func TestAuthenticator_InvalidateIsNoOpForNonInvalidatableSource(t *testing.T) {
	auth := NewAuthenticator(nonInvalidatableSource{})
	// Must not panic; a source that cannot invalidate stays usable.
	auth.Invalidate()
}
