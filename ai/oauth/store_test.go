package oauth

import (
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
