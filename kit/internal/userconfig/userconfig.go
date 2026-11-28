// Package userconfig names nib's files in the user's config directory,
// which a repository cannot write to.
package userconfig

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/latebit-io/nib/ai/brand"
)

// Path is name in nib's directory under the user's config directory.
func Path(name string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config dir: %w", err)
	}
	return filepath.Join(dir, brand.ConfigDirName, name), nil
}
