// Package demarkus implements memory.Store by shelling out to the demarkus CLI binary.
package demarkus

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/latebit-io/junto/engine/memory"
)

// maxOutputBytes caps stdout and stderr from the demarkus CLI.
// Prevents unbounded memory growth from large documents or noisy output.
const maxOutputBytes = 10 << 20 // 10 MB

// cmdTimeout is the maximum time a single demarkus CLI invocation may run.
const cmdTimeout = 10 * time.Second

// Adapter implements memory.Store using the demarkus CLI binary.
type Adapter struct {
	binPath   string // path to demarkus binary (.project/bin/demarkus)
	serverURL string // mark://localhost:{port}
	token     string // raw auth token for write ops
}

// New creates an Adapter that shells out to the demarkus CLI at binPath.
// serverURL is the mark:// URL of the running demarkus-server.
// token is the raw auth token for publish/append operations.
func New(binPath, serverURL, token string) *Adapter {
	return &Adapter{
		binPath:   binPath,
		serverURL: serverURL,
		token:     token,
	}
}

// Fetch retrieves a document by path.
func (a *Adapter) Fetch(ctx context.Context, path string) (memory.Document, error) {
	args := a.baseArgs(path)
	return a.execDoc(ctx, path, args)
}

// Publish creates or updates a document.
func (a *Adapter) Publish(ctx context.Context, path string, body string, expectedVersion int) (memory.Document, error) {
	args := a.writeArgs("PUBLISH", path, body, expectedVersion)
	return a.execDoc(ctx, path, args)
}

// Append adds content to an existing document.
func (a *Adapter) Append(ctx context.Context, path string, body string, expectedVersion int) (memory.Document, error) {
	args := a.writeArgs("APPEND", path, body, expectedVersion)
	return a.execDoc(ctx, path, args)
}

// baseArgs returns the common flags for a read-only request.
func (a *Adapter) baseArgs(path string) []string {
	return []string{"-v", "-insecure", "-no-cache", a.serverURL + path}
}

// writeArgs returns the flags for a PUBLISH or APPEND request.
func (a *Adapter) writeArgs(method, path, body string, expectedVersion int) []string {
	args := []string{
		"-v", "-insecure", "-no-cache",
		"-X", method,
		"-body", body,
		"-expected-version", strconv.Itoa(expectedVersion),
	}
	if a.token != "" {
		args = append(args, "-auth", a.token)
	}
	args = append(args, a.serverURL+path)
	return args
}

// execDoc runs the CLI and parses the response into a Document.
func (a *Adapter) execDoc(ctx context.Context, path string, args []string) (memory.Document, error) {
	stdout, stderr, cmdErr := a.run(ctx, args)
	if err := checkStatus(stderr, cmdErr); err != nil {
		return memory.Document{}, err
	}
	meta := parseStatusLine(stderr)
	return memory.Document{
		Path:     path,
		Body:     stdout,
		Version:  meta.version,
		Modified: meta.modified,
	}, nil
}

// List returns document paths under a directory.
func (a *Adapter) List(ctx context.Context, path string) ([]string, error) {
	args := []string{
		"-v",
		"-insecure",
		"-no-cache",
		"-X", "LIST",
		a.serverURL + path,
	}
	stdout, stderr, cmdErr := a.run(ctx, args)
	if err := checkStatus(stderr, cmdErr); err != nil {
		return nil, err
	}

	// LIST returns a markdown index page with links like:
	//   - [doc.md](doc.md)
	// Extract the link targets from (path) patterns.
	var paths []string
	for line := range strings.Lines(stdout) {
		// Find markdown link: [text](path)
		start := strings.Index(line, "](")
		if start < 0 {
			continue
		}
		rest := line[start+2:]
		end := strings.IndexByte(rest, ')')
		if end < 0 {
			continue
		}
		p := rest[:end]
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// run executes the demarkus CLI with the given arguments and returns stdout, stderr, and error.
// Output is capped at maxOutputBytes per stream to prevent unbounded memory growth.
// The caller's context is used for cancellation; cmdTimeout is applied as an upper bound.
func (a *Adapter) run(ctx context.Context, args []string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, a.binPath, args...)

	outBuf := &cappedBuffer{limit: maxOutputBytes}
	errBuf := &cappedBuffer{limit: maxOutputBytes}
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf

	err = cmd.Run()

	if outBuf.overflow || errBuf.overflow {
		return outBuf.String(), errBuf.String(), fmt.Errorf("memory: CLI output exceeded %d bytes", maxOutputBytes)
	}
	return outBuf.String(), errBuf.String(), err
}

// cappedBuffer is a bytes.Buffer that stops accepting writes after limit bytes.
// Excess data is silently discarded and the overflow flag is set.
type cappedBuffer struct {
	buf      strings.Builder
	written  int
	limit    int
	overflow bool
}

// Write implements io.Writer. Writes beyond the limit are silently dropped.
func (c *cappedBuffer) Write(p []byte) (int, error) {
	remaining := c.limit - c.written
	if remaining <= 0 {
		c.overflow = true
		return len(p), nil // pretend we consumed it so exec doesn't error
	}
	if len(p) > remaining {
		p = p[:remaining]
		c.overflow = true
	}
	n, err := c.buf.Write(p)
	c.written += n
	return len(p), err // report original len consumed
}

// String returns the captured output.
func (c *cappedBuffer) String() string {
	return c.buf.String()
}

// metadata holds parsed values from the verbose status line.
type metadata struct {
	status   string
	version  int
	modified string
	parseErr error // non-nil if a key=value field was malformed
}

// parseStatusLine parses the verbose stderr header from demarkus -v.
// Format: [status] key=value key=value ...
func parseStatusLine(stderr string) metadata {
	// Take only the first line — stderr may contain other output.
	line := stderr
	if idx := strings.IndexByte(stderr, '\n'); idx >= 0 {
		line = stderr[:idx]
	}
	line = strings.TrimSpace(line)

	var m metadata

	// Extract status from brackets: [ok], [not-found], etc.
	if strings.HasPrefix(line, "[") {
		end := strings.IndexByte(line, ']')
		if end > 0 {
			m.status = line[1:end]
			line = strings.TrimSpace(line[end+1:])
		}
	}

	// Parse remaining key=value pairs.
	for part := range strings.FieldsSeq(line) {
		key, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch key {
		case "version":
			v, err := strconv.Atoi(val)
			if err != nil {
				m.parseErr = fmt.Errorf("parse version %q: %w", val, err)
			}
			m.version = v
		case "modified":
			m.modified = val
		}
	}
	return m
}

// checkStatus inspects the verbose status line and returns an error for
// non-ok statuses. Demarkus exits 0 even on logical errors (not-found,
// conflict, unauthorized) — the status line is the source of truth.
func checkStatus(stderr string, cmdErr error) error {
	meta := parseStatusLine(stderr)

	// Map protocol-level error statuses to sentinel errors.
	switch meta.status {
	case "not-found":
		return fmt.Errorf("%w: %s", memory.ErrNotFound, strings.TrimSpace(stderr))
	case "conflict":
		return fmt.Errorf("%w: %s", memory.ErrConflict, strings.TrimSpace(stderr))
	case "unauthorized":
		return fmt.Errorf("%w: %s", memory.ErrAuth, strings.TrimSpace(stderr))
	case "error", "server-error":
		return fmt.Errorf("%w: %s", memory.ErrServer, strings.TrimSpace(stderr))
	}

	// For "ok" or missing status, the process must have exited cleanly.
	if cmdErr != nil {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			return fmt.Errorf("memory: demarkus command failed: %w", cmdErr)
		}
		return fmt.Errorf("memory: demarkus command failed: %s: %w", msg, cmdErr)
	}

	// Require an explicit [ok] status — missing status line means
	// the CLI output format changed or was truncated.
	if meta.status != "ok" {
		return fmt.Errorf("memory: unexpected response (no status line): %s", strings.TrimSpace(stderr))
	}

	// Reject malformed metadata on success responses.
	if meta.parseErr != nil {
		return fmt.Errorf("memory: malformed response: %w", meta.parseErr)
	}

	return nil
}
