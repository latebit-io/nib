package oauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/ai/internal/atomicjson"
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

// Put stores a token for a provider and persists to disk.
// The in-memory map is only updated after the write succeeds.
func (s *Store) Put(id ProviderID, tok *Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, existed := s.data[id]
	s.data[id] = tok
	if err := s.save(); err != nil {
		if existed {
			s.data[id] = prev
		} else {
			delete(s.data, id)
		}
		return err
	}
	return nil
}

// Delete removes a token for a provider and persists to disk.
// The in-memory map is only updated after the write succeeds.
func (s *Store) Delete(id ProviderID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, existed := s.data[id]
	if !existed {
		return nil
	}
	delete(s.data, id)
	if err := s.save(); err != nil {
		s.data[id] = prev // roll back
		return err
	}
	return nil
}

// HasToken reports whether a valid (non-expired) token exists for the provider.
func (s *Store) HasToken(id ProviderID) bool {
	t := s.Get(id)
	return t.Valid()
}

// load reads the store from disk. Missing files are silently ignored.
func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return json.Unmarshal(data, &s.data)
}

// save writes the store to disk atomically, creating parent directories
// as needed. Owner-only permissions: the file holds credentials.
func (s *Store) save() error {
	return atomicjson.Write(s.path, s.data, 0o700, 0o600)
}
