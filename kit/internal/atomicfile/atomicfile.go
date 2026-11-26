// Package atomicfile replaces a file's content in one step, so a crash
// leaves either the old file or the new one, never half of either.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Write puts data at path, creating its directory with dirPerm. The
// data is written beside the target, synced, then renamed over it.
func Write(path string, data []byte, dirPerm, filePerm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // gone already after a successful rename
	if err := write(tmp, data, filePerm); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// write fills and closes tmp. The sync matters: without it a crash
// after the rename can leave an empty file under the final name.
func write(tmp *os.File, data []byte, perm os.FileMode) error {
	_, err := tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(perm)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	return err
}
