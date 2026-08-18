// Package atomicjson writes JSON files atomically (temp file + rename) so a
// crash mid-write never leaves a truncated config or credential store.
package atomicjson

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Write marshals v as indented JSON and atomically replaces path with it,
// creating parent directories with dirPerm and the file with filePerm.
// The temp file lives beside path so the rename stays on one filesystem.
func Write(path string, v any, dirPerm, filePerm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("create dir %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, filePerm); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup of orphaned temp file
		return fmt.Errorf("rename %s → %s: %w", tmp, path, err)
	}
	return nil
}
