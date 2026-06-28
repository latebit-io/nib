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

// withinDir reports whether target resolves inside base, guarding
// catalog-relative paths against escape. It is symlink-aware: a lexical
// `filepath.Rel` check alone can be defeated by a symlink that sits under
// base textually but dereferences outside it, so both base and target are
// resolved with [filepath.EvalSymlinks] before the containment test.
//
// base must exist (it is always a freshly-fetched checkout at the call
// sites). target may not exist yet (e.g. the final source dir before
// promotion); in that case its deepest existing ancestor is resolved and
// the non-existent remainder re-appended, so a symlinked ancestor is
// still caught while a not-yet-created leaf is allowed. Any resolution
// failure is treated as "not contained" — fail closed.
func withinDir(base, target string) bool {
	rb, err := filepath.EvalSymlinks(base)
	if err != nil {
		return false
	}
	rt, err := resolveExisting(target)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rb, rt)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

// resolveExisting resolves symlinks in path, tolerating a not-yet-created
// leaf: it walks up to the deepest ancestor that exists, resolves that
// with [filepath.EvalSymlinks], and re-appends the missing tail. This
// keeps a symlinked ancestor honest while allowing a target whose final
// component has not been written yet.
func resolveExisting(path string) (string, error) {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved, nil
	}
	parent, leaf := filepath.Split(path)
	parent = filepath.Clean(parent)
	if parent == path { // reached the root without an existing ancestor
		return "", os.ErrNotExist
	}
	rp, err := resolveExisting(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(rp, leaf), nil
}
