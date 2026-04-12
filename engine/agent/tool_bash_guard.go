package agent

import (
	"fmt"
	"regexp"
	"strings"
)

// File-write guard for the bash tool.
//
// The LLM has dedicated edit_file / write_file tools that go through the
// approval flow and are platform-agnostic. Shell commands that redirect
// output to files bypass that flow and are not portable (BSD vs GNU head,
// sed -i behavior, etc.). This guard catches the common accidental patterns
// and returns an error message that steers the model toward the right tool.
//
// This is a heuristic, not a sandbox. Known bypass vectors that require a
// full shell parser to detect (accepted risks):
//   - Command substitution: sed $(echo -i) 's/x/y/' file.go
//   - Variable indirection: f=file.go; echo x > $f
//   - eval / source: eval "echo x > file.go"
//   - tee with safe first arg: tee /dev/null file2 (only first target checked)
//
// Defense-in-depth comes from the system prompt + this guard + the approval
// flow on edit_file.

var (
	// redirectRe matches output redirects: >, >>, or >| followed by a target token.
	redirectRe = regexp.MustCompile(`>(?:>|\|)?\s*(\S+)`)

	// inPlaceEditRe matches sed/perl in-place flags: -i, combined short flags
	// (-pi, -ni, -Ei, -0777pi), flags with backup suffixes (-ibak, -i.bak),
	// and the GNU long form --in-place. [^|;&]* prevents matching flags in a
	// different command after a pipe or semicolon.
	inPlaceEditRe = regexp.MustCompile(`\b(sed|perl)\b[^|;&]*(?:--in-place|-[a-zA-Z0-9]*i[a-zA-Z0-9.]*)\b`)

	// teeRe matches tee with an optional -a flag followed by a file target.
	teeRe = regexp.MustCompile(`\btee\s+(?:-a\s+)?(\S+)`)
)

// safeRedirectTarget returns true if the redirect target does not write to a
// project file. Targets like /dev/null, fd dups (&1), /tmp/, and process
// substitutions are considered safe.
func safeRedirectTarget(target string) bool {
	// Strip shell quotes so > "/tmp/file" is recognized as a /tmp/ target.
	target = strings.Trim(target, `"'`)

	switch {
	case strings.HasPrefix(target, "&"):
		return true // fd dup: >&1, >&2
	case strings.HasPrefix(target, "/dev/"):
		return true // /dev/null, /dev/stdout, /dev/stderr, /dev/fd/N
	case strings.HasPrefix(target, "/tmp/"):
		return true // scratch space outside the project
	case strings.HasPrefix(target, "("),
		strings.HasPrefix(target, ">("):
		return true // process substitution: >(cmd)
	default:
		return false
	}
}

// fileWriteGuard checks whether a shell command attempts to write files via
// mechanisms that bypass the edit approval flow. Returns an error message if
// the command should be blocked, or an empty string if allowed.
func fileWriteGuard(command string) string {
	// In-place editing: sed -i, perl -i
	if m := inPlaceEditRe.FindString(command); m != "" {
		return fmt.Sprintf(
			"Error: command modifies files in-place (%s), bypassing the edit approval flow.\n"+
				"Use edit_file or write_file instead.",
			strings.TrimSpace(m),
		)
	}

	// tee to a project file
	if m := teeRe.FindStringSubmatch(command); m != nil {
		if !safeRedirectTarget(m[1]) {
			return fmt.Sprintf(
				"Error: command writes to %s via tee, bypassing the edit approval flow.\n"+
					"Use edit_file or write_file instead.",
				m[1],
			)
		}
	}

	// Output redirects to non-safe targets
	for _, m := range redirectRe.FindAllStringSubmatch(command, -1) {
		target := m[1]
		if !safeRedirectTarget(target) {
			return fmt.Sprintf(
				"Error: command writes to %s via shell redirect, bypassing the edit approval flow.\n"+
					"Use edit_file or write_file instead.\n"+
					"If you need a scratch file, redirect to /tmp/ instead.",
				target,
			)
		}
	}

	return ""
}
