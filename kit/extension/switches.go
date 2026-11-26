package extension

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/kit/internal/filecap"
)

// maxSwitchesBytes caps the configuration file; it holds two short lists.
const maxSwitchesBytes = 64 << 10

// Switches is one level's choice of extensions: configuration, a
// session's flags, or a command in a running session.
type Switches struct {
	// Enable switches extensions on, by name.
	Enable []string `json:"enable"`
	// Disable switches extensions off, by name.
	Disable []string `json:"disable"`
}

// Then returns s with next applied on top: where both name an
// extension, the later level decides. Flags override configuration.
func (s Switches) Then(next Switches) Switches {
	drop := func(names, taken []string) []string {
		return slices.DeleteFunc(slices.Clone(names), func(n string) bool { return slices.Contains(taken, n) })
	}
	return Switches{
		Enable:  append(drop(s.Enable, next.Disable), next.Enable...),
		Disable: append(drop(s.Disable, next.Enable), next.Disable...),
	}
}

// DefaultSwitchesPath is where a binary keeps the person's choices:
// extensions.json in the user's config directory.
func DefaultSwitchesPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("extension: resolve user config dir: %w", err)
	}
	return filepath.Join(dir, brand.ConfigDirName, "extensions.json"), nil
}

// LoadSwitches reads a configuration file of the form
// {"enable": ["a"], "disable": ["b"]}. A missing file switches nothing.
func LoadSwitches(path string) (Switches, error) {
	var s Switches
	if _, err := filecap.ReadJSON(path, maxSwitchesBytes, "extension config", &s); err != nil {
		return Switches{}, fmt.Errorf("extension: %w", err)
	}
	return s, nil
}
