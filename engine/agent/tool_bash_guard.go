package agent

import (
	"fmt"
	"regexp"
	"strings"
)

// Bash command guards.
//
// The LLM has dedicated tools for editing (edit_file/write_file), searching
// (search_project), and structural questions (glob, list_files). Shell
// commands that bypass those tools either skip the approval flow, return
// non-portable results (BSD vs GNU sed/head/find), or do something
// destructive the dev didn't ask for. The guards here catch the common
// patterns and return an error message that steers the model toward the
// right tool — replacing prose-only "don't do X" rules in the system prompt
// with deterministic enforcement.
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

	// searchCmdRe matches code-search shell tools (grep / rg / ag / ack / find)
	// invoked at the start of a command segment. The (?:^|[;&|(]) anchor
	// catches both standalone invocations ("grep -r foo .") and chained ones
	// ("cd dir && grep -r foo ."). Lookahead-free because Go's RE2 doesn't
	// support lookbehind — we capture the optional separator into group 1
	// and the tool name into group 2 so the matcher can return the offending
	// tool for the error message.
	searchCmdRe = regexp.MustCompile(`(?:^|[;&|(])\s*\b(grep|rg|ripgrep|ag|ack|find)\b`)

	// destructiveOpRe matches commands that destroy local state without the
	// developer's explicit ask: `rm -r*f*` / `rm -f*r*` (recursive delete),
	// `git push`, `git checkout`, `git reset --hard`, `git clean -f`. These
	// are not theatre — accidental `git checkout .` or `rm -rf` from an
	// agent loses real work. Block at the tool layer so the dev can opt in
	// by typing the command themselves if they truly want it.
	//
	// rm: any flag run containing both 'r' and 'f' (in either order) is
	// recursive force-delete. Single-file `rm path` is not blocked — it
	// goes through the normal exit-code path and the dev can see what
	// happened. Two regexes cover the two shapes:
	//   - destructiveRmRe: r and f in the same `-` token (`-rf`, `-fr`,
	//     `--recursive --force`, etc.)
	//   - destructiveRmSplitRe: r and f in *separate* short-flag tokens
	//     before any command separator (`rm -r -f x`, `rm -f -r x`,
	//     `rm -v -r -f x`). The [^|;&]* between flags caps the scan to a
	//     single rm invocation so a later `rm -r` ; `cmd -f` does not
	//     cross-fire.
	destructiveRmRe      = regexp.MustCompile(`\brm\b\s+(?:-{1,2}[a-zA-Z]*(?:rf|fr|recursive)[a-zA-Z]*\b|--recursive\b[^|;&]*--force\b|--force\b[^|;&]*--recursive\b)`)
	destructiveRmSplitRe = regexp.MustCompile(`\brm\b[^|;&]*\s-[a-zA-Z]*r[a-zA-Z]*\b[^|;&]*\s-[a-zA-Z]*f[a-zA-Z]*\b|\brm\b[^|;&]*\s-[a-zA-Z]*f[a-zA-Z]*\b[^|;&]*\s-[a-zA-Z]*r[a-zA-Z]*\b`)

	// destructiveGitRe covers the git-history-loss patterns: push (publishes
	// state), checkout/switch (drops uncommitted changes), reset --hard
	// (drops history), clean -f (drops untracked files).
	destructiveGitRe = regexp.MustCompile(`\bgit\s+(?:push\b|checkout\b|switch\b|reset\b[^|;&]*--hard\b|clean\b[^|;&]*-[a-zA-Z]*f[a-zA-Z]*\b)`)
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

// firstLine returns the first newline-delimited line of s with surrounding
// whitespace trimmed. Useful for log fields where a multi-line guard
// message would otherwise wrap awkwardly — the first line of every guard
// message is the "Error: …" headline, which carries the classification
// without any of the multi-line "use X instead" guidance.
func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	return strings.TrimSpace(s)
}

// redactCommandPreview returns a short preview of command suitable for
// logging when a guard rejects it. The full command may contain secrets
// (env-var assignments, inline tokens, paths leaking workspace identity)
// that should not land in slog. We trim to redactPreviewBytes and add an
// ellipsis when the command was longer; the guard's class string (the
// `msg` returned by guardCommand) carries the *why* without needing the
// full command text. The preview exists purely so triage can grep logs
// for a recognisable shape ("rm -rf …") without exposing the rest.
//
// This is a heuristic, not a sanitiser. Secret-pattern masking (token
// detection, env-var stripping) is a separate hardening project — for
// now we just bound the surface.
func redactCommandPreview(command string) string {
	const redactPreviewBytes = 64
	command = strings.TrimSpace(command)
	if len(command) <= redactPreviewBytes {
		return command
	}
	return command[:redactPreviewBytes] + "…"
}

// guardCommand runs every active bash guard against a command and returns
// the first non-empty error message, or "" when the command passes them
// all. This is the single entry point [BashTool.Execute] calls before
// invoking the shell — adding a new guard means appending one line here
// rather than threading another check through the call site.
//
// Guard order is intentional: file-write checks first (highest false-
// positive risk if a destructive command coincidentally writes a file),
// then search redirects, then destructive-state ops. Every guard receives
// the unmodified command string; none of them mutate it.
func guardCommand(command string) string {
	if msg := fileWriteGuard(command); msg != "" {
		return msg
	}
	if msg := searchCommandGuard(command); msg != "" {
		return msg
	}
	if msg := destructiveCommandGuard(command); msg != "" {
		return msg
	}
	return ""
}

// searchCommandGuard blocks code-search shell tools (grep, rg, ripgrep, ag,
// ack, find) so the LLM is steered toward `search_project` and `glob`,
// which return structured file:line results, respect gitignore, and don't
// drag the conversation through a wall of unrelated bash output. Empty
// return means the command does not start with a blocked search tool.
//
// `find` is included because the typical LLM use of `find` is "find files
// matching a name pattern" — `glob` does that better and faster. `find`
// for filesystem ops (delete, exec) is rare from a coding agent and, if
// genuinely needed, the developer can run it themselves outside the agent.
func searchCommandGuard(command string) string {
	m := searchCmdRe.FindStringSubmatch(command)
	if m == nil {
		return ""
	}
	tool := m[1]
	return fmt.Sprintf(
		"Error: command starts with %q, which bypasses the structured search tools.\n"+
			"Use search_project for code search (returns file:line results, respects gitignore) "+
			"or glob for file-name patterns. If you must use bash for a one-off filesystem "+
			"operation, ask the developer to run it themselves.",
		tool,
	)
}

// destructiveCommandGuard blocks destructive operations the developer did
// not explicitly request: `rm -rf`, `git push`, `git checkout`, `git
// switch`, `git reset --hard`, `git clean -f`. The shared concern is "this
// command can lose real work the developer cared about, and an autonomous
// agent guessing wrong is more expensive than refusing."
//
// The dev can run any of these themselves outside the agent. The dev can
// also explicitly ask the agent ("force-push my branch to origin") — but
// that intent never reaches this layer, so we block first and let the
// failure message explain the policy. Better a one-turn retry-with-
// permission than an unrecoverable mistake.
func destructiveCommandGuard(command string) string {
	if destructiveRmRe.MatchString(command) || destructiveRmSplitRe.MatchString(command) {
		return "Error: command attempts a recursive force-delete (`rm -rf` or equivalent).\n" +
			"This is destructive and the developer has not explicitly asked for it. " +
			"Ask before retrying, or limit the delete to specific files without `-rf`."
	}
	if m := destructiveGitRe.FindString(command); m != "" {
		return fmt.Sprintf(
			"Error: command attempts a destructive git operation (%q).\n"+
				"Operations that publish state (push), drop uncommitted changes (checkout/switch), "+
				"or rewrite history (reset --hard, clean -f) are blocked unless the developer "+
				"explicitly asked. Ask before retrying.",
			strings.TrimSpace(m),
		)
	}
	return ""
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
