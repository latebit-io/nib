//go:build !unix

package filelock

// Lock is a no-op where flock is unavailable. nib's process tooling is
// unix-only (kit/memory/demarkus/server uses flock too), so concurrent
// writers on other platforms are unsupported.
func Lock(string) (unlock func(), err error) {
	return func() {}, nil
}
