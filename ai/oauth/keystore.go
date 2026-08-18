package oauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/ai/internal/atomicjson"
)

// KeyStore persists API keys entered via the TUI to disk.
// Keys are stored per-profile name. It is safe for concurrent use.
type KeyStore struct {
	path string
	mu   sync.RWMutex
	keys map[string]string
}

// NewKeyStore creates a KeyStore that reads/writes keys to the given file path.
// If the file exists, its contents are loaded. Missing files are not an error.
func NewKeyStore(path string) (*KeyStore, error) {
	s := &KeyStore{
		path: path,
		keys: make(map[string]string),
	}
	if err := s.load(); err != nil {
		return nil, fmt.Errorf("keystore: load %s: %w", path, err)
	}
	return s, nil
}

// DefaultKeyStorePath returns <UserConfigDir>/<brand.ConfigDirName>/keys.json
// (e.g. ~/.config/<brand>/keys.json on Linux). Returns empty string and an
// error if the user config directory cannot be resolved.
func DefaultKeyStorePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("keystore: resolve config dir: %w", err)
	}
	return filepath.Join(dir, brand.ConfigDirName, "keys.json"), nil
}

// Get returns the stored API key for a profile, or empty if none exists.
func (s *KeyStore) Get(profile string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.keys[profile]
}

// Put stores an API key for a profile and persists to disk.
// The in-memory map is only updated after the write succeeds.
// Returns an error if the key is empty or whitespace-only.
func (s *KeyStore) Put(profile, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("keystore: empty key for profile %q", profile)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, existed := s.keys[profile]
	s.keys[profile] = key
	if err := s.save(); err != nil {
		// Roll back.
		if existed {
			s.keys[profile] = prev
		} else {
			delete(s.keys, profile)
		}
		return err
	}
	return nil
}

// Delete removes an API key for a profile and persists to disk.
// The in-memory map is only updated after the write succeeds.
func (s *KeyStore) Delete(profile string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, existed := s.keys[profile]
	if !existed {
		return nil
	}
	delete(s.keys, profile)
	if err := s.save(); err != nil {
		s.keys[profile] = prev // roll back
		return err
	}
	return nil
}

// HasKey reports whether a non-empty key exists for the profile.
// Trims whitespace to stay consistent with Put's validation.
func (s *KeyStore) HasKey(profile string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return strings.TrimSpace(s.keys[profile]) != ""
}

// load reads the store from disk. Missing files are silently ignored.
func (s *KeyStore) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return json.Unmarshal(data, &s.keys)
}

// save writes the store to disk atomically, creating parent directories
// as needed. Owner-only permissions: the file holds API keys.
func (s *KeyStore) save() error {
	return atomicjson.Write(s.path, s.keys, 0o700, 0o600)
}
