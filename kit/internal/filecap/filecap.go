// Package filecap reads small on-disk files under a hard size cap, so a
// hostile or accidental giant file cannot exhaust memory. Shared by the
// skill and command loaders, which both read user-authored markdown from
// project- and user-global directories. Kept under kit/internal so it
// stays a private kit implementation detail.
package filecap

import (
	"fmt"
	"io"
	"os"
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
