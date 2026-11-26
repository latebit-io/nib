package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/kit/internal/atomicfile"
	"github.com/latebit-io/nib/kit/internal/filecap"
)

// TrustStore remembers which projects' server lists the person
// approved. Approval is tied to the file's content: a changed
// [ProjectConfigName] has to be approved again.
type TrustStore struct {
	path string
}

// trustFile is the store's on-disk form: project directory to the
// SHA-256 of the config file that was approved.
type trustFile struct {
	Projects map[string]string `json:"projects"`
}

// OpenTrustStore returns the store kept at path. The file is created
// on the first [TrustStore.Approve].
func OpenTrustStore(path string) *TrustStore {
	return &TrustStore{path: path}
}

// DefaultTrustPath is where a binary keeps the store: in the user's
// config directory, which a repository cannot write to.
func DefaultTrustPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("mcp: resolve user config dir: %w", err)
	}
	return filepath.Join(dir, brand.ConfigDirName, "mcp-trust.json"), nil
}

// Trusted reports whether projectDir's config file is the one approved.
// A project without the file has nothing to trust and reports false.
func (t *TrustStore) Trusted(projectDir string) (bool, error) {
	key, sum, err := fingerprint(projectDir)
	if err != nil || sum == "" {
		return false, err
	}
	file, err := t.read()
	if err != nil {
		return false, err
	}
	return file.Projects[key] == sum, nil
}

// Approve records projectDir's config file, as it is now, as approved,
// and returns what it then allows, so a host can show the person what
// they agreed to. A project without the file is an error.
func (t *TrustStore) Approve(projectDir string) (Config, error) {
	key, sum, err := fingerprint(projectDir)
	if err != nil {
		return Config{}, err
	}
	if sum == "" {
		return Config{}, fmt.Errorf("mcp: %s has no %s to trust", projectDir, ProjectConfigName)
	}
	file, err := t.read()
	if err != nil {
		return Config{}, err
	}
	file.Projects[key] = sum
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return Config{}, err
	}
	if err := atomicfile.Write(t.path, data, 0o700, 0o600); err != nil {
		return Config{}, fmt.Errorf("mcp: save trust store: %w", err)
	}
	return Discover(DiscoverOptions{ProjectDir: projectDir, TrustProject: true})
}

// read loads the store; a missing file is an empty store.
func (t *TrustStore) read() (trustFile, error) {
	file := trustFile{}
	if _, err := filecap.ReadJSON(t.path, maxConfigBytes, "MCP trust store", &file); err != nil {
		return file, fmt.Errorf("mcp: %w", err)
	}
	if file.Projects == nil {
		file.Projects = map[string]string{}
	}
	return file, nil
}

// fingerprint returns the store key for projectDir (its real path) and
// the hash of its config file; the hash is "" when there is no file.
func fingerprint(projectDir string) (key, sum string, err error) {
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return "", "", fmt.Errorf("mcp: %w", err)
	}
	// Resolved, so one project reached by two paths is one entry.
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	data, err := filecap.Read(filepath.Join(abs, ProjectConfigName), maxConfigBytes, "MCP config")
	if errors.Is(err, fs.ErrNotExist) {
		return abs, "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("mcp: %w", err)
	}
	hash := sha256.Sum256(data)
	return abs, hex.EncodeToString(hash[:]), nil
}
