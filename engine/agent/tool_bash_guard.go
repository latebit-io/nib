package agent

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
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
	// invoked at the start of a command segment. The (?:^|[;&|(\n]) anchor
	// catches standalone invocations ("grep -r foo ."), chained ones
	// ("cd dir && grep -r foo ."), AND newline-separated commands
	// inside a heredoc-style multi-line bash invocation
	// ("cd dir\ngrep TODO ."). The (?m) flag makes ^ match at the
	// start of any line, not just the start of the string — so a
	// command pasted into bash -c with embedded newlines is caught
	// at every line head, not only the first.
	//
	// The leading group is non-capturing (?: …); the tool name is the
	// only capturing group, so FindStringSubmatch returns
	// [full_match, tool] and callers read the tool from m[1]. (See
	// [searchCommandGuard] — `m[1]` is the offending tool name passed
	// into the error message.)
	searchCmdRe = regexp.MustCompile(`(?m)(?:^|[;&|(\n])\s*\b(grep|rg|ripgrep|ag|ack|find)\b`)

	// destructiveRmRe / destructiveRmSplitRe / destructiveGitRe match
	// commands that destroy local state without the developer's explicit
	// ask: `rm -rf` (recursive force-delete), `git push`, `git checkout`,
	// `git reset --hard`, `git clean -f`. These are not theatre —
	// accidental `git checkout .` or `rm -rf` from an agent loses real
	// work. Block at the tool layer so the dev can opt in by typing
	// the command themselves if they truly want it.
	//
	// rm: only RECURSIVE+FORCE deletes are blocked. `rm path` (single
	// file) and `rm --recursive path` (interactive recursive, prompts
	// before each file) are NOT blocked — they go through the normal
	// exit-code path. Three regexes cover the three shapes:
	//   - destructiveRmRe (short-flag combo): both 'r' and 'f' in the
	//     same `-` token (`-rf`, `-fr`, `-vrf`, `-vfr`).
	//   - destructiveRmRe (long-flag combo): both `--recursive` AND
	//     `--force` present in either order before any command
	//     separator. `--recursive` alone is allowed.
	//   - destructiveRmSplitRe: 'r' and 'f' in *separate* short-flag
	//     tokens (`rm -r -f x`, `rm -f -r x`, `rm -v -r -f x`). The
	//     [^|;&]* cap keeps a later `rm -r foo; cmd -f bar` from
	//     cross-firing.
	//
	// The short-flag pattern below requires r and f letters in the
	// SAME `-…` token (no `--` long-form here — long-form is handled
	// by the explicit `--recursive ... --force` branches that require
	// both). Examples: `-rf`, `-fr`, `-vrf`, `-r9f`. Counter-examples:
	// `--recursive` (long-form, only one of {r,f} present semantically),
	// `-r` alone, `-f` alone.
	destructiveRmRe      = regexp.MustCompile(`\brm\b\s+(?:-[a-zA-Z]*(?:rf|fr|r[a-zA-Z]+f|f[a-zA-Z]+r)[a-zA-Z]*\b|--recursive\b[^|;&]*--force\b|--force\b[^|;&]*--recursive\b)`)
	destructiveRmSplitRe = regexp.MustCompile(`\brm\b[^|;&]*\s-[a-zA-Z]*r[a-zA-Z]*\b[^|;&]*\s-[a-zA-Z]*f[a-zA-Z]*\b|\brm\b[^|;&]*\s-[a-zA-Z]*f[a-zA-Z]*\b[^|;&]*\s-[a-zA-Z]*r[a-zA-Z]*\b`)

	// destructiveGitRe covers the git-history-loss patterns: push
	// (publishes state), checkout/switch (drops uncommitted changes),
	// reset --hard (drops history), clean with -f or --force (drops
	// untracked files; -d/--dir extends to directories but the force
	// flag is what makes it destructive).
	//
	// `git clean` accepts both `-f` short form (with possible flag
	// combinations like `-fd`, `-fdx`) and `--force` long form, in any
	// position before a separator. Either shape is blocked.
	destructiveGitRe = regexp.MustCompile(`\bgit\s+(?:push\b|checkout\b|switch\b|reset\b[^|;&]*--hard\b|clean\b[^|;&]*(?:-[a-zA-Z]*f[a-zA-Z]*\b|--force\b))`)
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

// redactPreviewBytes is the byte cap on the trimmed preview returned by
// [redactCommandPreview]. 64 bytes is enough to keep the recognisable
// shape ("rm -rf build/", "echo … > out.txt") for triage without
// exposing the rest of an inline-secret command.
const redactPreviewBytes = 64

// envAssignmentRe matches an inline shell environment-variable
// assignment of the form `KEY=VALUE`. The KEY half is restricted to the
// shell-conventional shape `[A-Za-z_][A-Za-z0-9_]*` so identifiers like
// `PATH`, `OPENAI_API_KEY`, `MY_TOKEN` match while file paths
// (`./script.sh`), URLs (`http://...`), and similar non-assignment
// tokens do not. The VALUE is everything up to the next whitespace.
//
// Word-boundary anchoring (\b) ensures we don't match the trailing half
// of unrelated tokens like `foo=bar` inside a shell argument value.
var envAssignmentRe = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)=(\S+)`)

// redactCommandPreview returns a short preview of command suitable for
// logging when a guard rejects it. The full command may contain secrets
// (env-var assignments, inline tokens, paths leaking workspace identity)
// that should not land in slog. We mask, trim to [redactPreviewBytes],
// and add an ellipsis when the command was longer; the guard's class
// string (the `msg` returned by guardCommand) carries the *why* without
// needing the full command text. The preview exists purely so triage
// can grep logs for a recognisable shape ("rm -rf …") without exposing
// the rest.
//
// Masking covers ONE high-confidence shape: leading or inline
// `KEY=VALUE` env-var assignments (e.g. `OPENAI_API_KEY=sk-... cmd`).
// The VALUE is replaced with `<redacted>`. This catches the most common
// leak path on bash invocations and over-redacts harmlessly when KEY is
// non-sensitive (`PATH=/usr/bin foo` becomes `PATH=<redacted> foo` in
// logs only — the LLM still sees the full command via the tool result).
//
// Out of scope (intentionally — push back on the broader review ask):
//
//   - API-key shape detection (sk-…, ant-…, hf_…, ghp_…). Each
//     provider has a different format; pattern lists go stale and
//     produce false negatives on novel shapes. Real prevention is at
//     the source (don't put keys in shell args).
//   - Authorization header parsing (`-H "Authorization: Bearer …"`).
//     Requires real shell tokenisation: quoted args, escapes,
//     line-continuations. A regex that handles all that ends up bigger
//     than a tokeniser.
//
// If those become important, build a dedicated sanitiser package with
// its own test surface — don't bolt patterns onto this preview helper.
//
// Truncation is rune-aware: when the byte cap lands inside a multibyte
// UTF-8 sequence (e.g. a CJK glyph in a path, an em-dash in an
// argument), the cut backs up to the last rune boundary <=
// redactPreviewBytes so the returned string is always valid UTF-8.
// Mirrors the truncation pattern at engine/agent/prompt.go:51 and :63.
//
// Order: TrimSpace → mask → truncate. Masking must happen before
// truncation so the cap can't slice through the middle of a secret
// VALUE and leave half exposed.
func redactCommandPreview(command string) string {
	command = strings.TrimSpace(command)
	command = envAssignmentRe.ReplaceAllString(command, "$1=<redacted>")
	if len(command) <= redactPreviewBytes {
		return command
	}
	cut := redactPreviewBytes
	for cut > 0 && !utf8.RuneStart(command[cut]) {
		cut--
	}
	return command[:cut] + "…"
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
