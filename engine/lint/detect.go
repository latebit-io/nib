package lint

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Detect returns the default set of linters for projectRoot, chosen by
// project-language heuristics. Returns nil when no suitable linter is
// available — callers fall back to user-configured lint commands (see
// FromShellCommands) or skip lint entirely.
func Detect(projectRoot string) []Linter {
	if goModExists(projectRoot) {
		if _, err := exec.LookPath("golangci-lint"); err == nil {
			slog.Info("lint: auto-detected golangci-lint for Go project")
			return []Linter{&GolangciLinter{}}
		}
		if _, err := exec.LookPath("go"); err == nil {
			slog.Info("lint: auto-detected go vet (golangci-lint not on PATH)")
			return []Linter{&GoVetLinter{}}
		}
	}

	// Lua: first-class adapter deferred. Fall back to RawLinter wrapping
	// luacheck so auto-detection still works for Lua projects — less
	// structured than Go linting but preserves current behavior.
	if isLuaProject(projectRoot) {
		if _, err := exec.LookPath("luacheck"); err == nil {
			slog.Info("lint: auto-detected luacheck for Lua project (raw adapter)")
			return []Linter{&RawLinter{Command: "luacheck {file}"}}
		}
		slog.Debug("lint: Lua project detected but luacheck not on PATH")
	}

	return nil
}

// FromShellCommands wraps each user-configured shell command in a RawLinter.
// Returns nil when the input is empty so callers can compare against nil
// rather than checking length.
func FromShellCommands(cmds []string) []Linter {
	if len(cmds) == 0 {
		return nil
	}
	linters := make([]Linter, 0, len(cmds))
	for _, c := range cmds {
		linters = append(linters, &RawLinter{Command: c})
	}
	return linters
}

// DetectPerFile returns linters that operate safely on a single isolated
// file — useful for the pre-approval validator stage which writes the
// candidate's After content to a temp file (no package context). Used
// alongside [Detect]; the latter returns the full per-task set
// (golangci-lint, go vet) which need real package layout.
//
// Today's per-file safe set: luacheck for Lua projects. Add new entries
// when an adapter is verified to behave correctly with no surrounding
// package — gofmt is the obvious next candidate. Returning nil means
// "no per-file linter for this project"; the validator stage skips
// itself in that case via Applicable.
func DetectPerFile(projectRoot string) []Linter {
	if isLuaProject(projectRoot) {
		if _, err := exec.LookPath("luacheck"); err == nil {
			slog.Info("lint: per-file luacheck available for Lua project")
			return []Linter{&RawLinter{Command: "luacheck {file}"}}
		}
		slog.Debug("lint: Lua project detected but luacheck not on PATH (per-file)")
	}
	return nil
}

// PerFileShellCommands wraps each user-configured shell command in a
// RawLinter, but skips commands that cannot safely run against an
// isolated single file. A command must contain "{file}" to qualify —
// commands that only reference {dir} (or neither placeholder) operate
// on a whole directory and would mis-report when fed a temp directory
// holding only the candidate's content.
//
// Returns nil when no qualifying commands are present so callers can
// distinguish "user configured no per-file lint" from "ran clean."
func PerFileShellCommands(cmds []string) []Linter {
	if len(cmds) == 0 {
		return nil
	}
	var linters []Linter
	for _, c := range cmds {
		if !containsFilePlaceholder(c) {
			continue
		}
		linters = append(linters, &RawLinter{Command: c})
	}
	return linters
}

// containsFilePlaceholder reports whether cmd references the {file}
// placeholder, marking it safe for per-file dispatch.
func containsFilePlaceholder(cmd string) bool {
	// Use a simple substring check — RawLinter uses the same approach.
	// We deliberately do not parse the shell command; the placeholder
	// substring is sufficient for classification.
	return strings.Contains(cmd, "{file}")
}

func goModExists(projectRoot string) bool {
	_, err := os.Stat(filepath.Join(projectRoot, "go.mod"))
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	slog.Warn("lint: stat go.mod failed", "root", projectRoot, "err", err)
	return false
}

// statMarker checks whether root/name exists. Returns:
//   - (true, true)  — marker present
//   - (false, true) — marker not present
//   - (false, false) — stat failed for a reason other than not-exist; the
//     caller should fail closed rather than pretend the file wasn't there.
func statMarker(root, name string) (found, ok bool) {
	_, err := os.Stat(filepath.Join(root, name))
	if err == nil {
		return true, true
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, true
	}
	slog.Warn("lint: stat probe failed", "root", root, "name", name, "err", err)
	return false, false
}

// isLuaProject reports whether projectRoot looks like a Lua project. Shallow
// (root-level) check — deep walks are wasted work for a signal the developer
// can override via explicit style config.
func isLuaProject(projectRoot string) bool {
	if found, ok := statMarker(projectRoot, ".luacheckrc"); found || !ok {
		return found
	}
	if found, ok := statMarker(projectRoot, "main.lua"); found || !ok {
		return found
	}
	matches, err := filepath.Glob(filepath.Join(projectRoot, "*.lua"))
	if err != nil {
		slog.Warn("lint: Lua project glob failed", "root", projectRoot, "err", err)
		return false
	}
	return len(matches) > 0
}
