// Package memory defines the port interface for structured, versioned memory.
// Implementations execute operations against a Mark Protocol server.
package memory

import "errors"

// Sentinel errors for memory operations.
var (
	// ErrNotFound indicates the requested document does not exist.
	ErrNotFound = errors.New("memory: document not found")
	// ErrConflict indicates an optimistic concurrency conflict
	// (the document's version has changed since it was last fetched).
	ErrConflict = errors.New("memory: version conflict")
	// ErrAuth indicates the request was rejected due to missing or invalid auth.
	ErrAuth = errors.New("memory: unauthorized")
	// ErrServer indicates an unexpected server-side failure.
	ErrServer = errors.New("memory: server error")
)

// Document represents a fetched memory document.
type Document struct {
	// Path is the document path (e.g. "/index.md").
	Path string
	// Body is the markdown content.
	Body string
	// Version is the current version number (increments on each publish/append).
	Version int
	// Modified is an RFC 3339 timestamp of the last modification.
	Modified string
}

// Store is the port interface for structured memory.
// Implementations execute operations against a Mark Protocol server.
type Store interface {
	// Fetch retrieves a document by path.
	Fetch(path string) (Document, error)

	// Publish creates or updates a document. Uses optimistic concurrency:
	// expectedVersion=0 for create, >0 for update (must match current version).
	Publish(path string, body string, expectedVersion int) (Document, error)

	// Append adds content to an existing document.
	// expectedVersion must be >= 1 (document must exist).
	Append(path string, body string, expectedVersion int) (Document, error)

	// List returns document paths under a directory.
	List(path string) ([]string, error)
}
