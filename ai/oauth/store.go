package oauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/ai/internal/atomicjson"
	"github.com/latebit-io/nib/ai/internal/filelock"
)

// Store persists OAuth tokens to disk. It is safe for concurrent use.
type Store struct {
	path string
	mu   sync.RWMutex
	data map[ProviderID]*Token
}

// NewStore creates a Store that reads/writes tokens to the given file path.
// If the file exists, its contents are loaded. Missing files are not an error.
func NewStore(path string) (*Store, error) {
	s := &Store{
		path: path,
		data: make(map[ProviderID]*Token),
	}
	if err := s.load(); err != nil {
		return nil, fmt.Errorf("oauth store: load %s: %w", path, err)
	}
	return s, nil
}

// DefaultStorePath returns <UserConfigDir>/<brand.ConfigDirName>/auth.json
// (e.g. ~/.config/<brand>/auth.json on Linux). Returns empty string and an
// error if the user config directory cannot be resolved.
func DefaultStorePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("oauth: resolve config dir: %w", err)
	}
	return filepath.Join(dir, brand.ConfigDirName, "auth.json"), nil
}

// Get returns the stored token for a provider, or nil if none exists.
func (s *Store) Get(id ProviderID) *Token {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.data[id]
	if t == nil {
		return nil
	}
	// Return a copy to prevent mutation.
	cp := *t
	return &cp
}

// Put stores a copy of tok for a provider and persists to disk.
func (s *Store) Put(id ProviderID, tok *Token) error {
	if tok == nil {
		return fmt.Errorf("oauth store: nil token for %s", id)
	}
	cp := *tok
	return s.mutate(func(data map[ProviderID]*Token) bool {
		data[id] = &cp
		return true
	})
}

// Delete removes a token for a provider and persists to disk.
func (s *Store) Delete(id ProviderID) error {
	return s.DeleteIf(id, func(Token) bool { return true })
}

// DeleteIf removes the provider's token only when match accepts a copy of
// the current on-disk token, so a token another goroutine or process just
// stored is never removed by mistake.
func (s *Store) DeleteIf(id ProviderID, match func(Token) bool) error {
	return s.mutate(func(data map[ProviderID]*Token) bool {
		prev, ok := data[id]
		if !ok || !match(*prev) {
			return false
		}
		delete(data, id)
		return true
	})
}

// Reload re-reads the file so tokens another process wrote become visible
// to Get. On error the cached tokens are kept.
func (s *Store) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// mutate applies fn to the freshly reloaded file contents and saves the
// result, holding both the in-process mutex and a cross-process file lock,
// so a stale cache never overwrites another process's write. fn reports
// whether it changed anything; the cache rolls back if the save fails.
func (s *Store) mutate(fn func(data map[ProviderID]*Token) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := filelock.Lock(s.path + ".lock")
	if err != nil {
		return fmt.Errorf("oauth store: %w", err)
	}
	defer unlock()
	if err := s.load(); err != nil {
		return fmt.Errorf("oauth store: reload %s: %w", s.path, err)
	}
	prev := maps.Clone(s.data)
	if !fn(s.data) {
		return nil
	}
	if err := s.save(); err != nil {
		s.data = prev
		return err
	}
	return nil
}

// HasToken reports whether a valid (non-expired) token exists for the provider.
func (s *Store) HasToken(id ProviderID) bool {
	t := s.Get(id)
	return t.Valid()
}

// load replaces the cache with the file's contents; a missing file is an
// empty store. On error the cache is left unchanged. Caller holds s.mu.
func (s *Store) load() error {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.data = make(map[ProviderID]*Token)
		return nil
	}
	if err != nil {
		return err
	}
	data := make(map[ProviderID]*Token)
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}
	// A hand-edited {"openai": null} must not survive as a nil entry.
	maps.DeleteFunc(data, func(_ ProviderID, t *Token) bool { return t == nil })
	s.data = data
	return nil
}

// save writes the store to disk atomically, creating parent directories
// as needed. Owner-only permissions: the file holds credentials.
func (s *Store) save() error {
	return atomicjson.Write(s.path, s.data, 0o700, 0o600)
}
