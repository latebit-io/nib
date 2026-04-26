// Package runconfig loads the project's smoke-run configuration from
// `.project/run.json`, falling back to language-aware defaults when no
// file is present.
//
// Trust boundary: the file is the developer's stated intent for "what
// should we run to smoke-test this project?". Commands defined here
// bypass the agent's [agent.fileWriteGuard] because legitimate smoke
// targets (`make build && ./bin/foo`, `go run ./cmd/...`) frequently
// touch the filesystem. Reviewers must treat `.project/run.json` like
// any other build script — it is committed to the repo and goes
// through normal code review.
package runconfig

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// DefaultTimeout is the wall-clock cap when the project config does not
// override it. Smoke runs that take longer than this should configure
// an explicit higher timeout.
const DefaultTimeout = 30 * time.Second

// EffectiveTimeout returns the timeout the runner will actually
// enforce, applying the [DefaultTimeout] fallback when r.Timeout is
// zero or negative. Callers that need to report or compare against
// "the timeout that was used" must go through this helper rather than
// reading r.Timeout directly — production code reaches Resolved via
// [Load] which already defaults the field, but tests and future
// caller paths constructing Resolved values directly would otherwise
// see zero and report misleading values like "Timed out after 0s".
func EffectiveTimeout(r Resolved) time.Duration {
	if r.Timeout <= 0 {
		return DefaultTimeout
	}
	return r.Timeout
}

// Config is the on-disk shape of `.project/run.json`. Fields are
// lowercase JSON keys to match the rest of junto's project config
// surface.
type Config struct {
	// SmokeCommand is the shell command (passed to `sh -c`) that
	// runs the project's smoke target. Empty falls back to the
	// language-default resolution chain.
	SmokeCommand string `json:"smoke_command,omitempty"`

	// TimeoutMS overrides [DefaultTimeout] when positive. Values
	// ≤ 0 are ignored.
	TimeoutMS int `json:"timeout_ms,omitempty"`

	// Disabled, when true, suppresses smoke runs even if a command
	// was resolvable. Useful for projects that have no runnable
	// entry point (libraries) or that intentionally skip smoke
	// testing.
	Disabled bool `json:"disabled,omitempty"`
}

// Resolved is the post-detection configuration ready for the runner.
// It always has a non-empty Command unless Skipped is set with a
// SkipReason.
type Resolved struct {
	// Command is the smoke command resolved against project files
	// (Makefile target, language default). Empty when Skipped.
	Command string

	// Source identifies how Command was resolved: "config" (explicit
	// in run.json), "make-smoke", "make-run", "go-run", "lua-main",
	// "python-main", "node-main".
	Source string

	// Timeout is the effective wall-clock cap.
	Timeout time.Duration

	// Skipped is true when no smoke command could be resolved and
	// the runner should not run. SkipReason explains why.
	Skipped bool

	// SkipReason is set when Skipped — typically "disabled in
	// .project/run.json" or "no runnable entry point detected."
	SkipReason string
}

// Load reads `.project/run.json` from projectRoot and resolves the
// smoke command, applying language-default fallbacks when the config
// file is absent or omits `smoke_command`. Returns a Resolved that is
// always safe to consult — Skipped semantics replace nil-checking.
func Load(projectRoot string) Resolved {
	cfg := loadFile(projectRoot)
	timeout := DefaultTimeout
	if cfg.TimeoutMS > 0 {
		timeout = time.Duration(cfg.TimeoutMS) * time.Millisecond
	}

	if cfg.Disabled {
		return Resolved{Skipped: true, SkipReason: "disabled in .project/run.json", Timeout: timeout}
	}

	if cfg.SmokeCommand != "" {
		return Resolved{Command: cfg.SmokeCommand, Source: "config", Timeout: timeout}
	}

	cmd, source := detectDefault(projectRoot)
	if cmd == "" {
		return Resolved{
			Skipped:    true,
			SkipReason: "no smoke command in .project/run.json and no language default detected",
			Timeout:    timeout,
		}
	}
	return Resolved{Command: cmd, Source: source, Timeout: timeout}
}

// loadFile reads `.project/run.json`. Missing file returns the zero
// value (handled as "use defaults"). Parse errors are logged and
// also return the zero value — a syntactically broken config should
// not fail-closed and prevent smoke testing entirely.
func loadFile(projectRoot string) Config {
	path := filepath.Join(projectRoot, ".project", "run.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("runconfig: cannot read run.json", "path", path, "err", err)
		}
		return Config{}
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		slog.Warn("runconfig: invalid run.json JSON", "path", path, "err", err)
		return Config{}
	}
	return cfg
}

// detectDefault resolves a smoke command for projects that have no
// `.project/run.json`. The resolution order is: Makefile `smoke`
// target → Makefile `run` target → language default (Go, Lua,
// Python, JS). Returns ("", "") when nothing is detected; the caller
// surfaces that as a skip with reason.
//
// Each language path is gated on the project containing a marker file
// the developer would recognise; a Lua project without `main.lua`
// won't be assumed runnable as "lua main.lua" because the assumption
// would silently fail at run time and produce confusing trace output.
func detectDefault(projectRoot string) (cmd, source string) {
	if hasMakefileTarget(projectRoot, "smoke") {
		return "make smoke", "make-smoke"
	}
	if hasMakefileTarget(projectRoot, "run") {
		return "make run", "make-run"
	}

	// Go: require BOTH a module marker (go.mod) AND a main.go at the
	// project root. `go run ./...` was the obvious shape but it
	// errors for library modules (no main package) and multi-binary
	// repos (the `cmd/foo`, `cmd/bar` pattern). Mirroring the other
	// language defaults — which all check for a `main.<ext>` at root
	// — keeps the heuristic uniform: anything more complex should
	// configure `.project/run.json` or a Makefile target, both of
	// which already win over this auto-detection branch.
	if exists(projectRoot, "go.mod") && exists(projectRoot, "main.go") {
		if _, err := exec.LookPath("go"); err == nil {
			return "go run .", "go-run"
		}
	}
	if exists(projectRoot, "main.lua") {
		if _, err := exec.LookPath("love"); err == nil {
			return "love .", "love-run"
		}
		if _, err := exec.LookPath("lua"); err == nil {
			return "lua main.lua", "lua-main"
		}
	}
	if exists(projectRoot, "main.py") {
		if _, err := exec.LookPath("python3"); err == nil {
			return "python3 main.py", "python-main"
		}
	}
	if exists(projectRoot, "package.json") {
		if _, err := exec.LookPath("node"); err == nil {
			if exists(projectRoot, "main.js") {
				return "node main.js", "node-main"
			}
		}
	}
	return "", ""
}

// hasMakefileTarget reports whether projectRoot/Makefile defines a
// target named name. Uses a coarse text scan rather than full Make
// parsing — sufficient for the .PHONY-style targets junto cares about
// and avoids pulling in a Makefile parser.
func hasMakefileTarget(projectRoot, name string) bool {
	data, err := os.ReadFile(filepath.Join(projectRoot, "Makefile"))
	if err != nil {
		return false
	}
	// A target appears as "name:" at the start of a line. Scan
	// line-by-line so we don't confuse it with a target inside a
	// recipe ("\tname:" prefixed by tab is a shell command).
	prefix := name + ":"
	for line := range splitLines(string(data)) {
		if len(line) > 0 && line[0] == '\t' {
			continue
		}
		if len(line) >= len(prefix) && line[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// splitLines yields each line of s without allocating a slice. Mirrors
// strings.Split(s, "\n") semantics — empty trailing line is omitted
// when s has a trailing newline.
func splitLines(s string) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		start := 0
		for i := 0; i < len(s); i++ {
			if s[i] == '\n' {
				if !yield(s[start:i]) {
					return
				}
				start = i + 1
			}
		}
		if start < len(s) {
			yield(s[start:])
		}
	}
}

// exists reports whether projectRoot/name is a file or directory.
func exists(projectRoot, name string) bool {
	_, err := os.Stat(filepath.Join(projectRoot, name))
	return err == nil
}
