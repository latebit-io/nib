// Package lint holds the pure helpers used by the agent's post-task
// lint review. The orchestration that runs the configured linters and
// injects findings into the next turn lives on the Agent (it reads
// state, sends events, mutates pendingLint); only the path-grouping
// and finding-rendering helpers live here so they can be unit-tested
// without an Agent.
package lint

import (
	"fmt"
	"path/filepath"
	"strings"

	enginelint "github.com/latebit-io/nib/engine/lint"
)

// GroupPathsByDir returns the unique edited file paths, unique
// containing directories, and a dir→files map. Each is deterministic
// in insertion order so downstream output is stable across runs.
//
// A task that edits three files in one package lints one directory,
// not three — that's the whole point of the dir-level grouping. The
// caller passes only file paths (not the richer taskEdit struct) so
// this helper has no dependency on agent-side types.
func GroupPathsByDir(paths []string) (files, dirs []string, byDir map[string][]string) {
	seenFile := make(map[string]bool)
	seenDir := make(map[string]bool)
	byDir = make(map[string][]string)
	for _, p := range paths {
		if seenFile[p] {
			continue
		}
		seenFile[p] = true
		files = append(files, p)
		dir := filepath.Dir(p)
		if !seenDir[dir] {
			seenDir[dir] = true
			dirs = append(dirs, dir)
		}
		byDir[dir] = append(byDir[dir], p)
	}
	return files, dirs, byDir
}

// FormatFindings renders structured [enginelint.Finding] values as
// plain text for injection into the LLM's next-turn pendingLint
// message. One finding per line; ANSI free; deterministic order
// (caller-supplied).
func FormatFindings(findings []enginelint.Finding) string {
	var b strings.Builder
	for i, f := range findings {
		if i > 0 {
			b.WriteByte('\n')
		}
		// path:line:col: [linter] message
		b.WriteString(f.Path)
		if f.Line > 0 {
			fmt.Fprintf(&b, ":%d", f.Line)
			if f.Col > 0 {
				fmt.Fprintf(&b, ":%d", f.Col)
			}
		}
		b.WriteString(": ")
		if f.Linter != "" {
			fmt.Fprintf(&b, "[%s] ", f.Linter)
		}
		b.WriteString(f.Message)
	}
	return b.String()
}
