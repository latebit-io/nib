// Package cmdallow persists the developer's always-allow rules for
// per-command bash approval. Rules use the [toolperm] grant syntax
// (`Bash(go test ./...)`) and live in `.project/bash-allow.json` so
// they are repo-scoped and hand-editable — a developer can widen an
// exact rule to a glob (`Bash(git status*)`) or opt a trusted repo out
// of prompting entirely with a bare `Bash` rule.
//
// The list is shared mutable state between two goroutines: the agent's
// tool-dispatch goroutine reads it ([List.Permits], via the command
// approval gate) while the frontend goroutine appends to it
// ([List.Add], the always-allow action). All access is mutex-guarded
// internally; callers hold one *List for the process lifetime.
package cmdallow

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/latebit-io/nib/kit/toolperm"
)

// fileName is the allowlist's on-disk name under `.project/`.
const fileName = "bash-allow.json"

// fileFormat is the JSON shape of `.project/bash-allow.json`.
type fileFormat struct {
	// Allow holds toolperm grant strings, e.g. "Bash(go test ./...)".
	Allow []string `json:"allow"`
}

// List is a mutable, persisted set of always-allow command rules.
// The zero value is unusable; construct with [Load]. A nil *List
// permits nothing and rejects Add — callers without a configured
// allowlist can hold nil safely.
type List struct {
	mu      sync.RWMutex
	path    string
	rules   []toolperm.Rule
	matcher *toolperm.Matcher
}

// Load reads `.project/bash-allow.json` under projectRoot. A missing
// file yields an empty list (the file is created on the first Add). A
// read or parse failure is logged and also yields an empty list —
// fail-closed: an unreadable allowlist must never auto-approve, it just
// means every command is proposed.
func Load(projectRoot string) *List {
	l := &List{path: filepath.Join(projectRoot, ".project", fileName)}
	data, err := os.ReadFile(l.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("cmdallow: cannot read allowlist", "path", l.path, "err", err)
		}
		l.rebuildLocked()
		return l
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		slog.Warn("cmdallow: invalid allowlist JSON, ignoring file", "path", l.path, "err", err)
		l.rebuildLocked()
		return l
	}
	for _, s := range f.Allow {
		r, err := toolperm.ParseRule(s)
		if err != nil {
			// Skip just the broken rule — the developer's other rules
			// keep working, and the log names the offender to fix.
			slog.Warn("cmdallow: skipping invalid rule", "rule", s, "err", err)
			continue
		}
		l.rules = append(l.rules, r)
	}
	l.rebuildLocked()
	return l
}

// Permits reports whether the command is always-allowed. Nil-safe: a
// nil list permits nothing. A command containing shell control
// operators (chaining, pipes, substitution) is permitted only by a
// bare `Bash` rule or a byte-identical stored rule — an argument glob
// can only be trusted against a single simple command (see
// [toolperm.HasShellControl]), but exact string equality authorizes
// precisely the approved compound and nothing else. Other compound
// commands fall through to the approval prompt.
func (l *List) Permits(command string) bool {
	if l == nil {
		return false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if toolperm.HasShellControl(command) {
		return l.hasBareAllowLocked() || l.hasExactLocked(command)
	}
	return l.matcher.Allows("bash", command)
}

// Add appends an exact always-allow rule for the command and persists
// the list. Exact by design: auto-generalizing (`git status` →
// `Bash(git *)`) would silently widen to `git push --force`; the
// developer widens rules by editing the JSON. A command the list
// already permits is not duplicated. Returns an error when the list is
// nil or the write fails — the caller decides whether to still approve
// the command once.
func (l *List) Add(command string) error {
	if l == nil {
		return errors.New("cmdallow: no allowlist configured")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	alreadyPermitted := l.hasExactLocked(command)
	if toolperm.HasShellControl(command) {
		alreadyPermitted = alreadyPermitted || l.hasBareAllowLocked()
	} else {
		alreadyPermitted = alreadyPermitted || l.matcher.Allows("bash", command)
	}
	if alreadyPermitted {
		return nil
	}
	rule := toolperm.Rule{Tool: "Bash", Arg: command}
	l.rules = append(l.rules, rule)
	l.rebuildLocked()
	if err := l.saveLocked(); err != nil {
		// Roll back the in-memory append so memory and disk stay in
		// agreement — a rule that silently vanishes on restart is worse
		// than a visible persist failure.
		l.rules = l.rules[:len(l.rules)-1]
		l.rebuildLocked()
		return err
	}
	return nil
}

// Rules returns the current rule set in canonical grant-string form,
// for display (e.g. --plugins introspection). Nil-safe.
func (l *List) Rules() []string {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return toolperm.Strings(l.rules)
}

// hasBareAllowLocked reports whether a bare `Bash` rule (no argument
// glob) is present — the explicit opt-out that also admits compound
// commands. Callers hold l.mu.
func (l *List) hasBareAllowLocked() bool {
	for _, r := range l.rules {
		if r.Arg == "" {
			return true
		}
	}
	return false
}

// hasExactLocked reports whether a stored rule's argument equals the
// command byte-for-byte. Deliberately NOT glob matching: this is the
// path that admits an always-allowed compound command, and exactness is
// what keeps it safe. Callers hold l.mu.
func (l *List) hasExactLocked(command string) bool {
	for _, r := range l.rules {
		if r.Arg == command {
			return true
		}
	}
	return false
}

// rebuildLocked reconstructs the matcher from the current rules.
// Callers hold l.mu (or own the List exclusively during Load).
func (l *List) rebuildLocked() {
	l.matcher = toolperm.New(l.rules, nil)
}

// saveLocked writes the canonical rule set to disk, creating
// `.project/` if needed. Callers hold l.mu.
func (l *List) saveLocked() error {
	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cmdallow: create %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(fileFormat{Allow: toolperm.Strings(l.rules)}, "", "  ")
	if err != nil {
		return fmt.Errorf("cmdallow: encode allowlist: %w", err)
	}
	if err := os.WriteFile(l.path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("cmdallow: write %s: %w", l.path, err)
	}
	return nil
}
