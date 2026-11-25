package llmconfig

import (
	"context"
	"errors"
	"os"
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

// CheckAuth relies on the wired Auth satisfying [llm.CredentialChecker].
var _ llm.CredentialChecker = (*oauth.Authenticator)(nil)

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
	expiredStore := func(refresh string) *oauth.Store {
		s, err := oauth.NewStore(filepath.Join(t.TempDir(), "auth.json"))
		if err != nil {
			t.Fatalf("new store: %v", err)
		}
		old := &oauth.Token{AccessToken: "old", RefreshToken: refresh, ExpiresAt: time.Now().Add(-time.Hour)}
		if err := s.Put(oauth.ProviderOpenAI, old); err != nil {
			t.Fatalf("seed expired token: %v", err)
		}
		return s
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
			name:     "expired token with refresh token stays wired",
			resolved: &Resolved{OAuthProvider: "openai"},
			store:    expiredStore("refresh"),
			want:     true,
			wantAuth: true,
		},
		{
			name:     "expired token without refresh token is not wired",
			resolved: &Resolved{OAuthProvider: "openai"},
			store:    expiredStore(""),
			want:     false,
			wantAuth: false,
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

// failingSource is a TokenSource that never yields a token.
type failingSource struct{ err error }

func (f failingSource) Token(context.Context) (*oauth.Token, error) { return nil, f.err }

func TestCheckAuth(t *testing.T) {
	errDead := errors.New("refresh rejected")
	tests := []struct {
		name     string
		resolved *Resolved
		want     error
	}{
		{"nil resolved", nil, nil},
		{"no authenticator", &Resolved{}, nil},
		{"static key auth cannot self-check", &Resolved{Auth: llm.StaticKeyAuth("k")}, nil},
		{"dead oauth source surfaces error", &Resolved{Auth: oauth.NewAuthenticator(failingSource{errDead})}, errDead},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.resolved.CheckAuth(context.Background()); !errors.Is(got, tc.want) {
				t.Errorf("CheckAuth = %v, want %v", got, tc.want)
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
			name:     "credentials already present are kept",
			resolved: &Resolved{Profile: "openrouter", apiKey: "sk-env"},
			store:    keyStore,
			want:     false,
			wantKey:  "sk-env",
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

// TestWireDefaultKeyStore points the user config dir at a temp dir, so
// the default key store path is the test's own.
func TestWireDefaultKeyStore(t *testing.T) {
	tests := []struct {
		name     string
		saved    map[string]string // profile -> key in the store
		corrupt  bool              // the store file is not JSON
		resolved *Resolved
		wantKey  string
		wantErr  bool
	}{
		{
			name:     "key saved for the profile is applied",
			saved:    map[string]string{"openrouter": "sk-saved"},
			resolved: &Resolved{Profile: "openrouter"},
			wantKey:  "sk-saved",
		},
		{
			name:     "a key from the environment keeps priority",
			saved:    map[string]string{"openrouter": "sk-saved"},
			resolved: &Resolved{Profile: "openrouter", apiKey: "sk-env"},
			wantKey:  "sk-env",
		},
		{
			name:     "nothing saved for the profile",
			saved:    map[string]string{"other": "sk-other"},
			resolved: &Resolved{Profile: "openrouter"},
		},
		{
			name:     "unreadable store is an error, not a silent miss",
			corrupt:  true,
			resolved: &Resolved{Profile: "openrouter"},
			wantErr:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			// Windows resolves the config dir from AppData alone.
			t.Setenv("AppData", home)
			path, err := oauth.DefaultKeyStorePath()
			if err != nil {
				t.Fatalf("DefaultKeyStorePath: %v", err)
			}
			if tc.corrupt {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
				if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			}
			if len(tc.saved) > 0 {
				seed, err := oauth.NewKeyStore(path)
				if err != nil {
					t.Fatalf("NewKeyStore: %v", err)
				}
				for profile, key := range tc.saved {
					if err := seed.Put(profile, key); err != nil {
						t.Fatalf("Put: %v", err)
					}
				}
			}

			store, err := WireDefaultKeyStore(tc.resolved)

			if (err != nil) != tc.wantErr || (store == nil) != tc.wantErr {
				t.Fatalf("store = %v, err = %v; want error: %v", store, err, tc.wantErr)
			}
			if tc.resolved.apiKey != tc.wantKey {
				t.Errorf("key = %q; want %q", tc.resolved.apiKey, tc.wantKey)
			}
		})
	}
}
