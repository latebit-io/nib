// Package mcpadapter implements memory.Store by calling the demarkus-mcp
// MCP server over stdio. It replaces the per-operation CLI subprocess
// adapter (engine/memory/demarkus) with a single long-lived MCP client.
package mcpadapter

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/latebit-io/nib/engine/mcp"
	"github.com/latebit-io/nib/engine/memory"
)

// caller is the narrow surface of mcp.Client used by the adapter. Defined
// locally so tests can substitute a stub without spinning up a subprocess.
type caller interface {
	CallToolResult(ctx context.Context, name string, args map[string]any) (mcp.ToolResult, error)
}

// Adapter implements memory.Store by invoking MCP tools on a demarkus-mcp server.
type Adapter struct {
	client caller
}

// New returns an Adapter backed by the given MCP client. The client must
// already be initialized (Initialize called) and connected to a demarkus-mcp
// server started with -host pointing at the project's demarkus-server.
func New(client *mcp.Client) *Adapter {
	return &Adapter{client: client}
}

// Fetch retrieves a document by path.
func (a *Adapter) Fetch(ctx context.Context, p string) (memory.Document, error) {
	r, err := a.client.CallToolResult(ctx, "mark_fetch", map[string]any{"url": p})
	if err != nil {
		return memory.Document{}, fmt.Errorf("memory: mark_fetch: %w", err)
	}
	return parseDocument(p, r)
}

// Publish creates or updates a document with optimistic concurrency.
func (a *Adapter) Publish(ctx context.Context, p, body string, expectedVersion int) (memory.Document, error) {
	args := map[string]any{
		"url":              p,
		"body":             body,
		"expected_version": expectedVersion,
	}
	r, err := a.client.CallToolResult(ctx, "mark_publish", args)
	if err != nil {
		return memory.Document{}, fmt.Errorf("memory: mark_publish: %w", err)
	}
	return parseDocument(p, r)
}

// Append adds content to the end of an existing document.
func (a *Adapter) Append(ctx context.Context, p, body string, expectedVersion int) (memory.Document, error) {
	args := map[string]any{
		"url":              p,
		"body":             body,
		"expected_version": expectedVersion,
	}
	r, err := a.client.CallToolResult(ctx, "mark_append", args)
	if err != nil {
		return memory.Document{}, fmt.Errorf("memory: mark_append: %w", err)
	}
	return parseDocument(p, r)
}

// List returns the document paths under a directory.
func (a *Adapter) List(ctx context.Context, dir string) ([]string, error) {
	r, err := a.client.CallToolResult(ctx, "mark_list", map[string]any{"url": dir})
	if err != nil {
		return nil, fmt.Errorf("memory: mark_list: %w", err)
	}
	if r.IsError {
		return nil, fmt.Errorf("%w: %s", memory.ErrServer, strings.TrimSpace(r.Text))
	}
	_, body, err := parseResult(r.Text)
	if err != nil {
		return nil, err
	}
	return extractLinks(dir, body), nil
}

// parseDocument turns a CallToolResult into a Document or a sentinel error.
// It handles both handler-reported errors (IsError == true) and protocol-level
// failures surfaced through the "status:" header.
func parseDocument(p string, r mcp.ToolResult) (memory.Document, error) {
	if r.IsError {
		return memory.Document{}, fmt.Errorf("%w: %s", memory.ErrServer, strings.TrimSpace(r.Text))
	}
	meta, body, err := parseResult(r.Text)
	if err != nil {
		return memory.Document{}, err
	}
	switch meta["status"] {
	case "ok", "created":
		version, verr := parseVersion(meta["version"])
		if verr != nil {
			return memory.Document{}, fmt.Errorf("memory: malformed response: %w", verr)
		}
		return memory.Document{
			Path:     p,
			Body:     body,
			Version:  version,
			Modified: meta["modified"],
		}, nil
	case "not-found":
		return memory.Document{}, fmt.Errorf("%w: %s", memory.ErrNotFound, p)
	case "conflict":
		return memory.Document{}, fmt.Errorf("%w: %s (server-version=%s)", memory.ErrConflict, p, meta["server-version"])
	case "unauthorized":
		return memory.Document{}, fmt.Errorf("%w: %s", memory.ErrAuth, p)
	case "archived":
		// Archived documents return their last version but the caller can't
		// distinguish from a live document without this error.
		return memory.Document{}, fmt.Errorf("%w: %s is archived", memory.ErrNotFound, p)
	case "":
		return memory.Document{}, fmt.Errorf("%w: missing status line in response", memory.ErrServer)
	default:
		return memory.Document{}, fmt.Errorf("%w: status=%s", memory.ErrServer, meta["status"])
	}
}

// parseResult splits a demarkus-mcp text response into the header map and body.
// The response is shaped as:
//
//	status: <word>
//	key: value
//	key: value
//	<blank line>
//	<body lines>
//
// The body starts on the first line after a blank separator. When no blank
// separator is present, the body is empty (e.g. error responses that are
// headers only).
func parseResult(text string) (headers map[string]string, body string, err error) {
	headers = make(map[string]string)
	idx := 0
	for idx < len(text) {
		nl := strings.IndexByte(text[idx:], '\n')
		var line string
		if nl < 0 {
			line = text[idx:]
			idx = len(text)
		} else {
			line = text[idx : idx+nl]
			idx += nl + 1
		}
		if line == "" {
			// Blank separator: remainder is the body.
			body = text[idx:]
			return headers, body, nil
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			return nil, "", fmt.Errorf("%w: malformed header line %q", memory.ErrServer, line)
		}
		headers[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}
	// No blank separator — headers only, no body.
	return headers, "", nil
}

// parseVersion converts a header version string to int. An empty value
// yields zero, matching demarkus behavior when the server omits the field
// (e.g. LIST responses).
func parseVersion(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parse version %q: %w", s, err)
	}
	return n, nil
}

// extractLinks pulls `- [text](target)` link targets from a LIST response body,
// resolving relative targets against the requested directory so callers get
// absolute paths usable with Fetch. Trailing slashes on directory entries are
// preserved (path.Join would strip them).
func extractLinks(dir, body string) []string {
	var paths []string
	for line := range strings.Lines(body) {
		start := strings.Index(line, "](")
		if start < 0 {
			continue
		}
		rest := line[start+2:]
		end := strings.IndexByte(rest, ')')
		if end < 0 {
			continue
		}
		target := rest[:end]
		if target == "" {
			continue
		}
		if !strings.HasPrefix(target, "/") {
			base := dir
			if !strings.HasSuffix(base, "/") {
				base += "/"
			}
			target = base + target
		}
		paths = append(paths, target)
	}
	return paths
}
