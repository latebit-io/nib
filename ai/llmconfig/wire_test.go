package llmconfig

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/oauth"
)

// The OAuth self-heal path depends on the wired Auth being recognizable as
// an [llm.CredentialInvalidator] so the provider can discard a revoked
// token after a 401. WireOAuth assigns *oauth.Authenticator to the
// resolved profile's Auth field; this compile-time assertion fails loudly
// if that type ever stops satisfying the invalidation port.
var _ llm.CredentialInvalidator = (*oauth.Authenticator)(nil)

func TestWireOAuth(t *testing.T) {
	dir := t.TempDir()
	store, err := oauth.NewStore(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	tok := &oauth.Token{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour)}
	if err := store.Put(oauth.ProviderOpenAI, tok); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	tests := []struct {
		name     string
		resolved *Resolved
		store    *oauth.Store
		want     bool
		wantAuth bool
	}{
		{
			name:     "openai with token sets Auth",
			resolved: &Resolved{OAuthProvider: "openai"},
			store:    store,
			want:     true,
			wantAuth: true,
		},
		{
			name:     "non-oauth profile is no-op",
			resolved: &Resolved{APIKeyEnv: "X"},
			store:    store,
			want:     false,
			wantAuth: false,
		},
		{
			name:     "missing token returns false",
			resolved: &Resolved{OAuthProvider: "copilot"},
			store:    store,
			want:     false,
			wantAuth: false,
		},
		{
			name:     "unknown provider returns false",
			resolved: &Resolved{OAuthProvider: "bogus"},
			store:    store,
			want:     false,
			wantAuth: false,
		},
		{
			name:     "nil store returns false",
			resolved: &Resolved{OAuthProvider: "openai"},
			store:    nil,
			want:     false,
			wantAuth: false,
		},
		{
			name:     "nil resolved returns false",
			resolved: nil,
			store:    store,
			want:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := WireOAuth(tc.resolved, tc.store)
			if got != tc.want {
				t.Errorf("WireOAuth = %v, want %v", got, tc.want)
			}
			if tc.resolved != nil && (tc.resolved.Auth != nil) != tc.wantAuth {
				t.Errorf("Auth set = %v, want %v", tc.resolved.Auth != nil, tc.wantAuth)
			}
		})
	}
}

func TestWireStoredKey(t *testing.T) {
	dir := t.TempDir()
	keyStore, err := oauth.NewKeyStore(filepath.Join(dir, "keys.json"))
	if err != nil {
		t.Fatalf("new key store: %v", err)
	}
	if err := keyStore.Put("openrouter", "sk-test"); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	if err := keyStore.Put("padded", "  sk-padded  "); err != nil {
		t.Fatalf("seed padded: %v", err)
	}

	tests := []struct {
		name     string
		resolved *Resolved
		store    *oauth.KeyStore
		want     bool
		wantKey  string
	}{
		{
			name:     "stored key applied",
			resolved: &Resolved{Profile: "openrouter"},
			store:    keyStore,
			want:     true,
			wantKey:  "sk-test",
		},
		{
			name:     "padded key trimmed",
			resolved: &Resolved{Profile: "padded"},
			store:    keyStore,
			want:     true,
			wantKey:  "sk-padded",
		},
		{
			name:     "missing profile returns false",
			resolved: &Resolved{Profile: "absent"},
			store:    keyStore,
			want:     false,
		},
		{
			name:     "empty profile name returns false",
			resolved: &Resolved{},
			store:    keyStore,
			want:     false,
		},
		{
			name:     "nil store returns false",
			resolved: &Resolved{Profile: "openrouter"},
			store:    nil,
			want:     false,
		},
		{
			name:     "oauth-only profile short-circuits with false",
			resolved: &Resolved{Profile: "openrouter", OAuthProvider: "openai"},
			store:    keyStore,
			want:     false,
			wantKey:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := WireStoredKey(tc.resolved, tc.store)
			if got != tc.want {
				t.Errorf("WireStoredKey = %v, want %v", got, tc.want)
			}
			if tc.resolved != nil && tc.resolved.apiKey != tc.wantKey {
				t.Errorf("apiKey = %q, want %q", tc.resolved.apiKey, tc.wantKey)
			}
		})
	}
}
