package a2a

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/latebit-io/nib/kit/internal/filecap"
)

// maxTokenBytes caps the token file; a token is a few dozen characters.
const maxTokenBytes = 1 << 10

// DefaultTokenPath is the server's bearer token in the user config directory.
func DefaultTokenPath() (string, error) {
	return configFile("a2a-token")
}

// Token returns the server's bearer token kept at path, creating it on
// first use. A file others may read is refused: the token is the lock.
func Token(path string) (string, error) {
	data, err := filecap.Read(path, maxTokenBytes, "A2A token")
	if errors.Is(err, fs.ErrNotExist) {
		token, err := newToken(path)
		if errors.Is(err, fs.ErrExist) {
			return Token(path) // another server made it first
		}
		return token, err
	}
	if err != nil {
		return "", fmt.Errorf("a2a: token: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("a2a: token: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("a2a: token file %s may be read by others; chmod 600 it", path)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("a2a: token file %s is empty", path)
	}
	return token, nil
}

// newToken writes a random token to path, which must not exist yet. It
// is written beside path first and linked in whole: a reader never sees
// it half written, and a token already there is never replaced.
func newToken(path string) (string, error) {
	token := rand.Text()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("a2a: token: %w", err)
	}
	f, err := os.CreateTemp(dir, ".a2a-token-*") // 0600
	if err != nil {
		return "", fmt.Errorf("a2a: token: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err := os.Remove(tmp); err != nil {
			slog.Warn("a2a: remove temporary token file", "path", tmp, "err", err)
		}
	}()
	_, werr := f.WriteString(token + "\n")
	if err := errors.Join(werr, f.Close()); err != nil {
		return "", fmt.Errorf("a2a: token: %w", err)
	}
	if err := os.Link(tmp, path); err != nil {
		return "", fmt.Errorf("a2a: token: %w", err)
	}
	return token, nil
}
