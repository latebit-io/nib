package truncate

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// stashSubdir is the directory under the project root where stashed
// truncation payloads land. Project-local (vs os.TempDir) so the LLM
// can reference paths via the standard read_file path-traversal guard.
const stashSubdir = ".project/tooltmp"

// stalePruneAge is how old a sibling run-id directory must be before
// [NewProjectStash] removes it. 24h is the lifetime the plan calls out.
const stalePruneAge = 24 * time.Hour

// ProjectStash is a [Sink] that writes stashed payloads to
// `<projectRoot>/.project/tooltmp/<runID>/<seq>-<label>.txt`.
//
// Files persist for the lifetime of the stash and are removed when
// [ProjectStash.Cleanup] is called (typically from the coding agent's
// Close path). Sibling run-id directories older than [stalePruneAge]
// are pruned at construction so crashed runs don't accumulate.
//
// A nil *ProjectStash is a valid [Sink]: [ProjectStash.Stash] reports
// an error and the caller's truncation marker falls back to the
// no-path shape.
type ProjectStash struct {
	projectRoot string
	runID       string
	dir         string // <projectRoot>/<stashSubdir>/<runID>
	created     atomic.Bool
	seq         atomic.Int64
}

// NewProjectStash constructs a stash rooted at projectRoot. The run-id
// is generated internally (8 hex chars from crypto/rand). The
// on-disk directory is NOT created until the first [ProjectStash.Stash]
// call — runs that never truncate any output leave the filesystem
// untouched.
//
// Stale sibling run-id directories (older than 24h) are pruned in a
// best-effort pass; pruning errors are logged but never returned.
func NewProjectStash(projectRoot string) (*ProjectStash, error) {
	if projectRoot == "" {
		return nil, fmt.Errorf("truncate: project root required")
	}
	id, err := newRunID()
	if err != nil {
		return nil, fmt.Errorf("truncate: generate run id: %w", err)
	}
	s := &ProjectStash{
		projectRoot: projectRoot,
		runID:       id,
		dir:         filepath.Join(projectRoot, stashSubdir, id),
	}
	pruneStale(filepath.Join(projectRoot, stashSubdir), id)
	return s, nil
}

// Stash writes content to a unique file in the stash directory and
// returns the file's path relative to the project root (the form the
// LLM passes to read_file). The label is sanitized into the filename
// so logs and ls output stay readable.
//
// A nil receiver returns an error so call sites can treat
// "no stash configured" and "stash failed" uniformly. The pure
// [Bytes]/[Lines] helpers already accept nil [Sink] directly and
// suppress this code path.
func (s *ProjectStash) Stash(label, content string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("truncate: nil stash")
	}
	if err := s.ensureDir(); err != nil {
		return "", fmt.Errorf("truncate: ensure stash dir: %w", err)
	}
	seq := s.seq.Add(1)
	name := fmt.Sprintf("%04d-%s.txt", seq, sanitizeLabel(label))
	abs := filepath.Join(s.dir, name)
	if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("truncate: write stash: %w", err)
	}
	rel, err := filepath.Rel(s.projectRoot, abs)
	if err != nil {
		// Should be unreachable — abs is built under projectRoot — but
		// fall back to the absolute path rather than poisoning the
		// marker with an error.
		return abs, nil
	}
	return rel, nil
}

// Cleanup removes the stash directory and every file in it. Idempotent;
// safe to call from a sync.Once-guarded Close path. Errors are logged
// at warn but never returned, so Close doesn't gain a new failure mode.
func (s *ProjectStash) Cleanup() {
	if s == nil {
		return
	}
	if !s.created.Load() {
		// Nothing was ever written — skip the RemoveAll syscall.
		return
	}
	if err := os.RemoveAll(s.dir); err != nil {
		slog.Warn("truncate: stash cleanup failed", "dir", s.dir, "err", err)
	}
}

// Dir reports the absolute directory the stash writes into. Exported
// for tests and observability; production callers should use the path
// returned by [ProjectStash.Stash].
func (s *ProjectStash) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

func (s *ProjectStash) ensureDir() error {
	if s.created.Load() {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	s.created.Store(true)
	return nil
}

// newRunID returns 8 hex characters of cryptographic randomness. The
// space is large enough that the 24h cleanup window will never see a
// collision in practice; small enough to keep filesystem paths readable.
func newRunID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// sanitizeLabel turns an arbitrary label into a short filesystem-safe
// suffix. Runs of non-alphanumerics collapse to "_"; empty labels
// become "stash".
func sanitizeLabel(label string) string {
	if label == "" {
		return "stash"
	}
	const max = 24
	buf := make([]byte, 0, len(label))
	prevSep := false
	for i := 0; i < len(label) && len(buf) < max; i++ {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9':
			buf = append(buf, c)
			prevSep = false
		default:
			if !prevSep && len(buf) > 0 {
				buf = append(buf, '_')
				prevSep = true
			}
		}
	}
	if len(buf) == 0 {
		return "stash"
	}
	return string(buf)
}

// pruneStale walks the stash root and removes child directories whose
// mtime is older than [stalePruneAge]. The current run-id is skipped
// even if its directory exists (we just created it, so it wouldn't
// trip the age cutoff, but the explicit skip makes the intent clear).
// Errors are logged and otherwise ignored.
func pruneStale(stashRoot, skipID string) {
	entries, err := os.ReadDir(stashRoot)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Debug("truncate: stash prune readdir", "dir", stashRoot, "err", err)
		}
		return
	}
	cutoff := time.Now().Add(-stalePruneAge)
	for _, e := range entries {
		if !e.IsDir() || e.Name() == skipID {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		victim := filepath.Join(stashRoot, e.Name())
		if err := os.RemoveAll(victim); err != nil {
			slog.Warn("truncate: stash prune remove", "dir", victim, "err", err)
		}
	}
}
