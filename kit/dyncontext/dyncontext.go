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
	// fencedRe matches a ```! … ``` block; the command is captured.
	fencedRe = regexp.MustCompile("(?s)```!\\s*\n(.*?)\n```")
	// inlineRe matches an inline !`cmd` directive.
	inlineRe = regexp.MustCompile("!`([^`]+)`")
)

// Expand replaces every dynamic-context directive in body with its gated
// output and returns the result. Fenced blocks are processed before
// inline directives. A nil matcher denies everything (fail closed); a nil
// runner turns every otherwise-permitted directive into an error marker
// rather than executing.
func Expand(ctx context.Context, body string, perm *toolperm.Matcher, runner Runner) string {
	if perm == nil {
		perm = toolperm.DenyAll()
	}
	repl := func(re *regexp.Regexp) func(string) string {
		return func(match string) string {
			cmd := strings.TrimSpace(re.FindStringSubmatch(match)[1])
			return renderDirective(ctx, cmd, perm, runner)
		}
	}
	body = fencedRe.ReplaceAllStringFunc(body, repl(fencedRe))
	body = inlineRe.ReplaceAllStringFunc(body, repl(inlineRe))
	return body
}

// HasDirectives reports whether body contains any dynamic-context
// directive — a cheap pre-check so callers can skip Expand (and its
// matcher/runner plumbing) for the common directive-free body.
func HasDirectives(body string) bool {
	return inlineRe.MatchString(body) || fencedRe.MatchString(body)
}

// renderDirective gates one command and returns the text to inline.
func renderDirective(ctx context.Context, cmd string, perm *toolperm.Matcher, runner Runner) string {
	if cmd == "" {
		return ""
	}
	if !perm.Allows(gateTool, cmd) {
		return fmt.Sprintf("[blocked: command %q is not permitted by this skill's tool grants]", cmd)
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
