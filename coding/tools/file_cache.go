package tools

import (
	"log/slog"
	"sync"
)

// FileCache is a concurrency-safe cache of file contents. The agent
// maintains its own view of file state, updated through tool reads
// and post-approval seeding, to avoid races with user edits.
type FileCache struct {
	mu    sync.Mutex
	files map[string]string
}

// NewFileCache creates an empty file cache.
func NewFileCache() *FileCache {
	return &FileCache{files: make(map[string]string)}
}

// Get returns the cached content for a path, if present.
func (c *FileCache) Get(path string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	content, ok := c.files[path]
	return content, ok
}

// Invalidate removes a path from the cache so the next read hits disk.
func (c *FileCache) Invalidate(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.files, path)
}

// Set updates the cached content for a path.
func (c *FileCache) Set(path, content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files[path] = content
}

// LoadOrRead returns cached content for canon, or reads from disk via
// reader and populates the cache on miss. The reader is zero-arg so
// callers capture the read path in a closure — this prevents accidental
// conflation of the cache key (canonical absolute path) with the
// reader's input path contract.
func (c *FileCache) LoadOrRead(canon string, reader func() (string, error)) (string, error) {
	if content, ok := c.Get(canon); ok {
		slog.Debug("file cache: hit", "path", canon, "content_len", len(content))
		return content, nil
	}
	content, err := reader()
	if err != nil {
		return "", err
	}
	slog.Debug("file cache: read from disk", "path", canon, "content_len", len(content))
	c.Set(canon, content)
	return content, nil
}

// Reset clears the cache and seeds it with the given file.
func (c *FileCache) Reset(path, content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files = map[string]string{path: content}
}
