package oauth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")

	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// Initially empty.
	if tok := store.Get(ProviderOpenAI); tok != nil {
		t.Fatal("expected nil token for empty store")
	}
	if store.HasToken(ProviderOpenAI) {
		t.Fatal("HasToken should be false for empty store")
	}

	// Store a token.
	tok := &Token{
		AccessToken:  "test-access",
		RefreshToken: "test-refresh",
		ExpiresAt:    time.Now().Add(time.Hour),
		AccountID:    "acct-123",
	}
	if err := store.Put(ProviderOpenAI, tok); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Read it back.
	got := store.Get(ProviderOpenAI)
	if got == nil {
		t.Fatal("expected non-nil token")
	}
	if got.AccessToken != "test-access" {
		t.Errorf("AccessToken = %q, want %q", got.AccessToken, "test-access")
	}
	if got.RefreshToken != "test-refresh" {
		t.Errorf("RefreshToken = %q, want %q", got.RefreshToken, "test-refresh")
	}
	if got.AccountID != "acct-123" {
		t.Errorf("AccountID = %q, want %q", got.AccountID, "acct-123")
	}
	if !store.HasToken(ProviderOpenAI) {
		t.Fatal("HasToken should be true after Put")
	}

	// Reload from disk — new Store instance.
	store2, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore (reload): %v", err)
	}
	got2 := store2.Get(ProviderOpenAI)
	if got2 == nil || got2.AccessToken != "test-access" {
		t.Fatalf("token not persisted: got %v", got2)
	}

	// Delete.
	if err := store2.Delete(ProviderOpenAI); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if store2.HasToken(ProviderOpenAI) {
		t.Fatal("HasToken should be false after Delete")
	}
}

func TestStoreGetReturnsCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")

	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	tok := &Token{AccessToken: "original"}
	if err := store.Put(ProviderOpenAI, tok); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got := store.Get(ProviderOpenAI)
	got.AccessToken = "mutated"

	// Original should be unchanged.
	got2 := store.Get(ProviderOpenAI)
	if got2.AccessToken != "original" {
		t.Errorf("Get returned mutable reference: token is %q, want %q", got2.AccessToken, "original")
	}
}

func TestStoreMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent", "auth.json")

	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore should not error for missing file: %v", err)
	}

	// Should work — creates dirs on first write.
	tok := &Token{AccessToken: "test"}
	if err := store.Put(ProviderCopilot, tok); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

func TestStoreDropsNullTokensOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(`{"openai":null}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if store.Get(ProviderOpenAI) != nil {
		t.Error("null entry loaded as a token")
	}
	if err := store.Delete(ProviderOpenAI); err != nil { // must not panic on the dropped entry
		t.Errorf("Delete: %v", err)
	}
}

func TestStorePutRejectsNilAndCopies(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Put(ProviderOpenAI, nil); err == nil {
		t.Error("Put(nil) succeeded; want error")
	}
	tok := &Token{AccessToken: "a"}
	if err := store.Put(ProviderOpenAI, tok); err != nil {
		t.Fatalf("Put: %v", err)
	}
	tok.AccessToken = "mutated"
	if got := store.Get(ProviderOpenAI).AccessToken; got != "a" {
		t.Errorf("stored token = %q; Put kept the caller's pointer", got)
	}
}

// openStore opens a second Store on path, standing in for another process
// with its own cache.
func openStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestStoreDeleteIfSeesAnotherProcessRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	a := openStore(t, path)
	if err := a.Put(ProviderOpenAI, &Token{AccessToken: "old", RefreshToken: "r1", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	b := openStore(t, path) // caches the stale r1 token
	if err := a.Put(ProviderOpenAI, &Token{AccessToken: "new", RefreshToken: "r2", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// b's refresh with r1 was rejected; its stale match must not delete r2.
	if err := b.DeleteIf(ProviderOpenAI, func(cur Token) bool { return cur.RefreshToken == "r1" }); err != nil {
		t.Fatalf("DeleteIf: %v", err)
	}
	if got := openStore(t, path).Get(ProviderOpenAI); got == nil || got.RefreshToken != "r2" {
		t.Errorf("on-disk token = %+v; stale process clobbered the rotated token", got)
	}
	if got := b.Get(ProviderOpenAI); got == nil || got.RefreshToken != "r2" {
		t.Errorf("b cache = %+v; want the rotated token after DeleteIf reload", got)
	}
}

func TestStorePutKeepsAnotherProcessEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	a, b := openStore(t, path), openStore(t, path)
	if err := a.Put(ProviderOpenAI, &Token{AccessToken: "o"}); err != nil {
		t.Fatalf("a.Put: %v", err)
	}
	if err := b.Put(ProviderCopilot, &Token{AccessToken: "c"}); err != nil {
		t.Fatalf("b.Put: %v", err)
	}
	fresh := openStore(t, path)
	if fresh.Get(ProviderOpenAI) == nil || fresh.Get(ProviderCopilot) == nil {
		t.Errorf("on-disk store lost an entry: openai=%v copilot=%v", fresh.Get(ProviderOpenAI), fresh.Get(ProviderCopilot))
	}
}
