// Package atomicjson writes JSON files atomically (unique temp file + fsync +
// rename) so a crash or a concurrent writer never leaves a truncated config
// or credential store.
package atomicjson

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Write marshals v as indented JSON and atomically replaces path with it,
// creating parent directories with dirPerm and the file with filePerm.
// The temp file lives beside path so the rename stays on one filesystem;
// its name is unique per call so concurrent writers to the same path each
// rename a complete file (last one wins, never a torn one).
func Write(path string, v any, dirPerm, filePerm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	data = append(data, '\n')

	// CreateTemp opens 0600; widen/narrow to filePerm before content lands.
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()        // may already be closed; error not actionable
			_ = os.Remove(tmpName) // best-effort cleanup of orphaned temp file
		}
	}()
	if err := tmp.Chmod(filePerm); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	// fsync before rename: otherwise a crash after the rename can leave the
	// new name pointing at a zero-length file.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s → %s: %w", tmpName, path, err)
	}
	return nil
}
