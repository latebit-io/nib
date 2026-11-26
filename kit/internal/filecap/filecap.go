// Package filecap reads small on-disk files under a hard size cap, so a
// hostile or accidental giant file cannot exhaust memory. Shared by the
// skill and command loaders, which both read user-authored markdown from
// project- and user-global directories. Kept under kit/internal so it
// stays a private kit implementation detail.
package filecap

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
)

// Read reads at most max bytes from the file at path, rejecting the file
// outright when it is larger. The Size() pre-check fails fast; the
// LimitReader is belt-and-suspenders against a file that grows between
// stat and read. kind labels the file in the size-exceeded error (e.g.
// "skill", "command").
//
// Callers are responsible for constraining path (the loaders build it
// from os.ReadDir entries under a configured root, so traversal is not
// reachable — entry names are single path components).
func Read(path string, max int64, kind string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // caller constrains path to a ReadDir-walked root; see callers.
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only open; close error is benign.
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > max {
		return nil, fmt.Errorf("%s is %d bytes, exceeds the %d-byte %s cap", path, info.Size(), max, kind)
	}
	return io.ReadAll(io.LimitReader(f, max))
}

// ErrTooLarge is in the chain of the error for a file over its cap.
var ErrTooLarge = errors.New("over the read cap")

// ErrNotRegular is in the chain of the error for a directory, pipe,
// socket or device.
var ErrNotRegular = errors.New("not a regular file")

// ReadRoot reads the regular file name inside root, refusing one over
// max bytes. Confinement is root's: no path or symlink leaves it.
func ReadRoot(root *os.Root, name string, max int64) ([]byte, error) {
	// Non-blocking: a read-only open of a FIFO otherwise waits for a
	// writer, and the regular-file check below would never run.
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only handle; nothing to flush
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w", name, ErrNotRegular)
	}
	// One byte past the cap: the size from Stat can be stale.
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s: %w (%d bytes allowed)", name, ErrTooLarge, max)
	}
	return data, nil
}

// ReadJSON decodes the optional file at path into v. A missing file
// leaves v alone and reports found false; kind labels a size error.
func ReadJSON(path string, max int64, kind string, v any) (found bool, err error) {
	data, err := Read(path, max, kind)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return true, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}
