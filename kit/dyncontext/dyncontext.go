// Package dyncontext expands the dynamic-context directives that Claude
// Code skill/command bodies embed — an inline “ !`cmd` “ or a fenced
// ` ```! ` block whose command output is inlined when the artifact is
// invoked.
//
// Every command is gated by a [toolperm.Matcher]: nib runs a directive
// only if the artifact's grants permit `Bash(<command>)`. A denied
// directive is replaced with a visible `[blocked: …]` marker and never
// executed. This is the point where a plugin's `allowed-tools` /
// `disallowed-tools` become real enforcement: a prompt-only skill (no
// Bash grant) yields a deny-all matcher, so its directives never run; a
// trusted plugin's shell skill runs only the commands its grants match.
//
// Execution itself is delegated to a [Runner] so the gate-and-inline
// logic is testable without a real shell.
package dyncontext

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/latebit-io/nib/kit/toolperm"
)

// gateTool is the tool name directives are matched against. Dynamic
// context is shell, so grants are evaluated as Bash(<command>).
const gateTool = "Bash"

// Runner executes a shell command and returns its combined output.
// Injected so [Expand] can be tested without spawning a process.
type Runner interface {
	Run(ctx context.Context, command string) (output string, err error)
}

var (
	// fencedExecRe matches an executable ```! … ``` block; the command
	// is captured.
	fencedExecRe = regexp.MustCompile("(?s)```!\\s*\n(.*?)\n```")
	// inlineRe matches an inline !`cmd` directive.
	inlineRe = regexp.MustCompile("!`([^`]+)`")
	// inlinePassRe matches EITHER a normal code fence (group 1) or an
	// inline directive (group 2). Used for the inline pass so an example
	// like !`git status` inside a documentation fence is left as literal
	// text rather than executed.
	inlinePassRe = regexp.MustCompile("(?s)(```.*?```)|!`([^`]+)`")
)

// Expand replaces every dynamic-context directive in body with its gated
// output and returns the result. Executable ```! blocks are processed
// first; then inline “ !`cmd` “ directives are expanded everywhere
// EXCEPT inside normal code fences (so documentation snippets stay
// literal). A nil matcher denies everything (fail closed); a nil runner
// turns every otherwise-permitted directive into an error marker.
func Expand(ctx context.Context, body string, perm *toolperm.Matcher, runner Runner) string {
	if perm == nil {
		perm = toolperm.DenyAll()
	}
	body = fencedExecRe.ReplaceAllStringFunc(body, func(match string) string {
		cmd := strings.TrimSpace(fencedExecRe.FindStringSubmatch(match)[1])
		return renderDirective(ctx, cmd, perm, runner)
	})
	body = inlinePassRe.ReplaceAllStringFunc(body, func(match string) string {
		sm := inlinePassRe.FindStringSubmatch(match)
		if sm[1] != "" {
			return match // a normal code fence — leave its contents verbatim
		}
		return renderDirective(ctx, strings.TrimSpace(sm[2]), perm, runner)
	})
	return body
}

// HasDirectives reports whether body contains any dynamic-context
// directive — a cheap pre-check so callers can skip Expand (and its
// matcher/runner plumbing) for the common directive-free body.
func HasDirectives(body string) bool {
	return inlineRe.MatchString(body) || fencedExecRe.MatchString(body)
}

// renderDirective gates one command and returns the text to inline.
func renderDirective(ctx context.Context, cmd string, perm *toolperm.Matcher, runner Runner) string {
	if cmd == "" {
		return ""
	}
	if !perm.Allows(gateTool, cmd) {
		return fmt.Sprintf("[blocked: command %q is not permitted by this skill's tool grants]", cmd)
	}
	// A matcher glob like `git *` matches the whole command string, so it
	// would also match a chained `git x; rm -rf y`. Reject shell control
	// operators outright so a grant cannot be escaped via chaining,
	// subshells, redirects, or command substitution — the command must be
	// a single simple invocation to run.
	if op, bad := shellControl(cmd); bad {
		return fmt.Sprintf("[blocked: command %q contains shell operator %q, not permitted in dynamic context]", cmd, op)
	}
	if runner == nil {
		return fmt.Sprintf("[unavailable: no command runner for %q]", cmd)
	}
	out, err := runner.Run(ctx, cmd)
	if err != nil {
		return fmt.Sprintf("[error running %q: %v]\n%s", cmd, err, strings.TrimRight(out, "\n"))
	}
	return strings.TrimRight(out, "\n")
}

// shellControl reports whether cmd contains a shell control operator that
// could chain, redirect, background, or substitute commands — the vectors
// by which a matcher glob (`git *`) could be escaped. It returns the
// offending token for a clear diagnostic.
func shellControl(cmd string) (string, bool) {
	if i := strings.Index(cmd, "$("); i >= 0 {
		return "$(", true
	}
	const operators = ";|&`<>()\n"
	if i := strings.IndexAny(cmd, operators); i >= 0 {
		return string(cmd[i]), true
	}
	return "", false
}
