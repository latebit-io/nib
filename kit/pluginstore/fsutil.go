package pluginstore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// swapDir atomically replaces final with staged, preserving the existing
// final until the replacement is in place. A naive "RemoveAll(final) then
// Rename(staged, final)" loses final entirely if the rename fails; this
// renames the old tree aside first and restores it on failure, so a
// promotion error never leaves the store referencing a missing directory.
func swapDir(staged, final string) error {
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return fmt.Errorf("pluginstore: create parent of %s: %w", final, err)
	}
	backup := final + ".old"
	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("pluginstore: clear stale backup %s: %w", backup, err)
	}
	hadExisting := false
	if _, err := os.Stat(final); err == nil {
		if err := os.Rename(final, backup); err != nil {
			return fmt.Errorf("pluginstore: back up %s: %w", final, err)
		}
		hadExisting = true
	}
	if err := os.Rename(staged, final); err != nil {
		if hadExisting {
			_ = os.Rename(backup, final) // best-effort restore of the prior tree
		}
		return fmt.Errorf("pluginstore: promote %s → %s: %w", staged, final, err)
	}
	if hadExisting {
		_ = os.RemoveAll(backup) // best-effort cleanup of the superseded tree
	}
	return nil
}

// withinDir reports whether target resolves inside base (after cleaning),
// guarding catalog-relative paths against `../` traversal that would
// escape the marketplace checkout. base and target should be absolute or
// share a base for the relative computation to be meaningful.
func withinDir(base, target string) bool {
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(target))
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}
