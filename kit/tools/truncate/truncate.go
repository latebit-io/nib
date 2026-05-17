// Package truncate provides the shared truncation contract used by every
// tool whose output is replayed to the LLM. The agent-token-efficiency
// plan caps each tool result at [DefaultMaxBytes] / [DefaultMaxLines]
// so a single oversized result cannot poison the conversation history
// for the remainder of the session.
//
// The package is split into two surfaces:
//
//   - Pure helpers ([Bytes], [Lines]) that decide whether truncation
//     fires, format the post-truncation string, and emit the
//     contract-shaped marker.
//   - A [Sink] interface tools may pass to the helpers so the full,
//     pre-truncation payload is stashed somewhere the LLM can recover
//     it from (the typical concrete implementation is [ProjectStash]).
//
// The marker format is intentionally a stable contract: the LLM is
// expected to read it, understand the cap fired, and either re-query
// with a narrower scope or follow the "Full output:" path to the
// stashed copy.
package truncate

import (
	"fmt"
	"log/slog"
	"strings"
)

// DefaultMaxBytes is the per-tool-result byte cap the plan locks in
// (50 KiB). Tools may override at the call site but should default
// here so the LLM's expectations stay consistent across tools.
const DefaultMaxBytes = 50 * 1024

// DefaultMaxLines is the per-tool-result line cap the plan locks in
// (2000 lines). Tools that work in line-oriented domains (read_file,
// search) prefer [Lines] over [Bytes].
const DefaultMaxLines = 2000

// Sink stashes the full, pre-truncation payload so the LLM can recover
// it from a marker-referenced path. A nil Sink is supported by [Bytes]
// and [Lines]: truncation still fires, but the marker omits the
// "Full output:" path and the LLM is expected to re-query.
type Sink interface {
	// Stash writes content somewhere reachable from the project root
	// and returns a path (project-relative or absolute, tool's choice)
	// the LLM can pass to read_file. The label is a short, filesystem-
	// safe hint (e.g. "bash", "search", "mcp_mark_fetch") used to
	// disambiguate concurrent stashes.
	Stash(label, content string) (path string, err error)
}

// Bytes truncates content to at most maxBytes bytes. When truncation
// fires:
//
//   - Content is sliced at a rune boundary as close to maxBytes as
//     possible (never exceeding it) so the result is valid UTF-8.
//   - If sink is non-nil, [Sink.Stash] is called with the full payload
//     and the resulting path is woven into the marker. A sink error
//     is logged at warn but does NOT fail the call — the LLM still
//     receives the truncated content with a marker omitting the path.
//   - A trailing marker is appended describing what was kept.
//
// When content fits under maxBytes, Bytes returns (content, false)
// untouched and never invokes the sink.
//
// label is the same parameter forwarded to [Sink.Stash]. Pass the
// tool's short name (e.g. "bash").
func Bytes(label, content string, maxBytes int, sink Sink) (out string, truncated bool) {
	total := len(content)
	if total <= maxBytes {
		return content, false
	}
	head := safeUTF8Prefix(content, maxBytes)
	stashPath := stashIfPossible(label, content, sink)
	marker := bytesMarker(len(head), total, stashPath)
	return head + marker, true
}

// Lines truncates content to at most maxLines lines. Behavior mirrors
// [Bytes]; the marker reports line counts instead of byte counts.
//
// A trailing empty line (the common shape of newline-terminated files)
// is not counted toward maxLines so a 2000-line file with a final
// newline is not flagged as truncated.
func Lines(label, content string, maxLines int, sink Sink) (out string, truncated bool) {
	lines := strings.Split(content, "\n")
	total := len(lines)
	// strings.Split on trailing newline produces a final empty entry.
	// Treat it as part of the previous line for cap-comparison purposes.
	effective := total
	if effective > 0 && lines[total-1] == "" {
		effective = total - 1
	}
	if effective <= maxLines {
		return content, false
	}
	head := strings.Join(lines[:maxLines], "\n") + "\n"
	stashPath := stashIfPossible(label, content, sink)
	marker := linesMarker(maxLines, effective, stashPath)
	return head + marker, true
}

// Marker formats the contract-shaped truncation marker. Exported so
// tests outside this package can match against it without
// re-implementing the format. The unit string is either "bytes" or
// "lines"; stashPath may be empty.
func Marker(showing, total int, unit, stashPath string) string {
	if stashPath == "" {
		return fmt.Sprintf("\n\n[Truncated: showing %d of %d %s.]", showing, total, unit)
	}
	return fmt.Sprintf("\n\n[Truncated: showing %d of %d %s. Full output: %s]", showing, total, unit, stashPath)
}

func bytesMarker(showing, total int, stashPath string) string {
	return Marker(showing, total, "bytes", stashPath)
}

func linesMarker(showing, total int, stashPath string) string {
	return Marker(showing, total, "lines", stashPath)
}

// stashIfPossible returns the sink's stash path or empty on nil sink /
// sink error. Errors are logged but never propagated — truncation must
// not fail the tool call.
func stashIfPossible(label, content string, sink Sink) string {
	if sink == nil {
		return ""
	}
	path, err := sink.Stash(label, content)
	if err != nil {
		slog.Warn("truncate: stash failed",
			"label", label,
			"bytes", len(content),
			"err", err)
		return ""
	}
	return path
}

// safeUTF8Prefix returns the longest prefix of s whose length is at
// most n bytes and which ends on a valid UTF-8 boundary. The agent
// re-parses tool output as UTF-8 in several places (estimator, edit
// proposal preview); a partial code point would corrupt all of them.
func safeUTF8Prefix(s string, n int) string {
	if n >= len(s) {
		return s
	}
	// Walk backward at most 3 bytes (max UTF-8 continuation length) to
	// find a rune boundary.
	for i := n; i > n-4 && i > 0; i-- {
		if isUTF8Start(s[i]) {
			return s[:i]
		}
	}
	return s[:n]
}

// isUTF8Start reports whether b begins a valid UTF-8 code point (i.e.
// is not a 10xxxxxx continuation byte).
func isUTF8Start(b byte) bool {
	return b&0xC0 != 0x80
}
