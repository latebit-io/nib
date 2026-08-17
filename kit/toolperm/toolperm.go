package toolperm

import (
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// Rule is one parsed tool-permission grant: a tool name and an optional
// argument glob. Arg == "" matches any argument (a bare tool grant like
// `Read`); a non-empty Arg is a glob matched against the invocation's
// argument string (e.g. the bash command, or a file path).
type Rule struct {
	// Tool is the tool name, e.g. "Bash" or "Read". Matched
	// case-insensitively so frontmatter casing does not silently fail.
	Tool string
	// Arg is the argument glob ("" matches any). `*` matches any run of
	// characters (including path separators), `?` matches one.
	Arg string
}

// shellTools are the tool names that denote shell/script execution. The
// trust gate uses [Rule.IsShell] / [AnyShell] to decide whether a grant
// requires per-plugin approval before it may run.
var shellTools = map[string]bool{
	"bash": true, "sh": true, "shell": true, "exec": true,
}

// IsShell reports whether the rule grants shell/script execution.
func (r Rule) IsShell() bool { return shellTools[strings.ToLower(r.Tool)] }

// String renders the rule back to its canonical grant form: "Tool" for a
// bare grant, "Tool(arg)" when an argument glob is present. Round-trips
// with [ParseRule] (modulo whitespace normalization).
func (r Rule) String() string {
	if r.Arg == "" {
		return r.Tool
	}
	return r.Tool + "(" + r.Arg + ")"
}

// Strings renders a rule set to canonical grant strings, suitable for
// re-emitting into frontmatter. Returns nil for an empty set so an
// omitempty YAML field stays absent.
func Strings(rules []Rule) []string {
	if len(rules) == 0 {
		return nil
	}
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = r.String()
	}
	return out
}

// AnyShell reports whether any rule in the set grants shell execution.
func AnyShell(rules []Rule) bool {
	for _, r := range rules {
		if r.IsShell() {
			return true
		}
	}
	return false
}

// ParseRule parses a single grant token such as "Bash(git *)" or "Read".
// Whitespace around the tool name and inside the parentheses is trimmed.
// A token with an opening but no closing parenthesis, or an empty tool
// name, is an error.
func ParseRule(s string) (Rule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Rule{}, fmt.Errorf("toolperm: empty rule")
	}
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return Rule{Tool: s}, nil
	}
	if !strings.HasSuffix(s, ")") {
		return Rule{}, fmt.Errorf("toolperm: rule %q has '(' without closing ')'", s)
	}
	tool := strings.TrimSpace(s[:open])
	arg := strings.TrimSpace(s[open+1 : len(s)-1])
	if tool == "" {
		return Rule{}, fmt.Errorf("toolperm: rule %q has no tool name", s)
	}
	if arg == "" {
		// Empty parentheses are a likely authoring mistake: an empty Arg
		// matches ANY argument, so "Bash()" would silently mean
		// unrestricted "Bash". Reject it so the intent is explicit — use a
		// bare tool name for "any argument".
		return Rule{}, fmt.Errorf("toolperm: rule %q has empty parentheses (use bare %q for any argument)", s, tool)
	}
	return Rule{Tool: tool, Arg: arg}, nil
}

// ParseField parses a frontmatter allowed-tools / disallowed-tools value,
// which may be a single string (whitespace/comma separated, e.g.
// "Bash(git *) Read") or a list of strings. Argument globs may contain
// spaces and commas inside their parentheses; the splitter respects
// parenthesis nesting so "Bash(git commit -m *)" stays one rule. Invalid
// rules are collected and returned as a joined error alongside the rules
// that did parse, so a caller can choose strict-or-lenient handling.
func ParseField(v any) ([]Rule, error) {
	var tokens []string
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		tokens = splitRules(t)
	case []string:
		for _, e := range t {
			tokens = append(tokens, splitRules(e)...)
		}
	case []any:
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("toolperm: list element %v is not a string", e)
			}
			tokens = append(tokens, splitRules(s)...)
		}
	default:
		return nil, fmt.Errorf("toolperm: unsupported allowed-tools type %T", v)
	}

	var rules []Rule
	var errs []error
	for _, tok := range tokens {
		r, err := ParseRule(tok)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		rules = append(rules, r)
	}
	if len(errs) > 0 {
		return rules, joinErrs(errs)
	}
	return rules, nil
}

// splitRules splits a permission spec on whitespace and commas that sit
// outside parentheses, so argument globs containing those characters are
// preserved as a single token.
func splitRules(s string) []string {
	var tokens []string
	var b strings.Builder
	depth := 0
	flush := func() {
		if b.Len() > 0 {
			tokens = append(tokens, b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '(':
			depth++
			b.WriteRune(r)
		case r == ')':
			if depth > 0 {
				depth--
			}
			b.WriteRune(r)
		case (r == ' ' || r == '\t' || r == '\n' || r == ',') && depth == 0:
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return tokens
}

// Matcher evaluates a tool invocation against an allow set and a deny
// set. The zero value denies everything; construct with [New].
type Matcher struct {
	allow []compiledRule
	deny  []compiledRule
}

// compiledRule pairs a Rule with its glob compiled once at [New] time,
// so Allows/PermitsArg do not rebuild a regexp per invocation.
type compiledRule struct {
	Rule
	re *regexp.Regexp // nil when Arg == "" (any argument) or the glob failed to compile
}

// New builds a Matcher from allow and deny rule sets. Argument globs
// are compiled here; a glob that fails to compile never matches (the
// glob syntax only emits QuoteMeta and .*/. so this is theoretical).
func New(allow, deny []Rule) *Matcher {
	return &Matcher{allow: compileRules(allow), deny: compileRules(deny)}
}

func compileRules(rules []Rule) []compiledRule {
	out := make([]compiledRule, 0, len(rules))
	for _, r := range rules {
		cr := compiledRule{Rule: r}
		if r.Arg != "" {
			re, err := globRegexp(r.Arg)
			if err != nil {
				slog.Warn("toolperm: glob failed to compile; rule never matches", "rule", r.String(), "err", err)
			}
			cr.re = re
		}
		out = append(out, cr)
	}
	return out
}

// DenyAll returns a matcher that permits nothing. It is the fail-closed
// default callers use when grant parsing fails: a dropped deny rule must
// never let an otherwise-broad allow through, so the safe response to a
// malformed grant set is to deny everything.
func DenyAll() *Matcher { return &Matcher{} }

// HasAllowList reports whether any allow rules are present. An empty
// allow list grants nothing — useful for callers that want to treat a
// no-grants artifact as prompt-only rather than running it through the
// matcher.
func (m *Matcher) HasAllowList() bool { return len(m.allow) > 0 }

// GrantsTool reports whether the tool is granted in any form — an allow
// rule names it, regardless of argument pattern. Used for tool-LEVEL
// filtering (does the child get this tool at all), distinct from
// [Matcher.Allows] which gates a specific invocation's arguments.
func (m *Matcher) GrantsTool(tool string) bool {
	for _, r := range m.allow {
		if strings.EqualFold(r.Tool, tool) {
			return true
		}
	}
	return false
}

// DeniesTool reports whether the tool is denied outright — a bare deny
// rule names it with no argument pattern (e.g. `Write`). An
// argument-scoped deny like `Bash(rm *)` does NOT deny the tool itself,
// only matching invocations, so it is not reported here.
func (m *Matcher) DeniesTool(tool string) bool {
	for _, r := range m.deny {
		if r.Arg == "" && strings.EqualFold(r.Tool, tool) {
			return true
		}
	}
	return false
}

// Allows reports whether the named tool may run with the given argument
// string. Deny rules take precedence: a single matching deny rule blocks
// the call regardless of the allow set. Otherwise the call is permitted
// only if some allow rule matches — an empty or non-matching allow set
// denies.
func (m *Matcher) Allows(tool, arg string) bool {
	for _, r := range m.deny {
		if r.matches(tool, arg) {
			return false
		}
	}
	for _, r := range m.allow {
		if r.matches(tool, arg) {
			return true
		}
	}
	return false
}

// PermitsArg reports whether a tool that has ALREADY been granted at the
// tool level may run with the given argument. It differs from [Allows] in
// the empty-allow-list case: [Allows] is a strict allowlist (no matching
// allow rule ⇒ deny), whereas PermitsArg permits a tool the allow set
// never names, gating only by argument patterns when the tool IS named.
//
// This is the right check for command-level enforcement of an already-
// admitted tool (e.g. gating a granted bash tool's command): a deny rule
// always blocks; an allow list that scopes the tool (`bash(git *)`)
// restricts it to matching arguments; a deny-only grant
// (`disallowedTools: bash(rm *)`) blocks only the matching commands.
func (m *Matcher) PermitsArg(tool, arg string) bool {
	for _, r := range m.deny {
		if r.matches(tool, arg) {
			return false
		}
	}
	if !m.GrantsTool(tool) {
		// The allow set never names this tool, so it imposes no
		// argument restriction on it — permit (deny rules already checked).
		return true
	}
	for _, r := range m.allow {
		if r.matches(tool, arg) {
			return true
		}
	}
	return false
}

// HasArgRules reports whether any rule (allow or deny) constrains the
// named tool by argument — a rule naming the tool with a non-empty Arg.
// Callers use it to decide whether a tool's invocations need
// argument-level scrutiny: e.g. whether a bash grant scopes commands
// (`Bash(git *)` / `disallowedTools: Bash(rm *)`), in which case a
// compound shell command must be rejected because the per-argument glob
// can only be trusted against a single simple command.
func (m *Matcher) HasArgRules(tool string) bool {
	for _, set := range [][]compiledRule{m.allow, m.deny} {
		for _, r := range set {
			if r.Arg != "" && strings.EqualFold(r.Tool, tool) {
				return true
			}
		}
	}
	return false
}

// HasShellControl reports whether a command contains shell operators
// that chain, pipe, background, or substitute commands — the constructs
// that let a second command ride along past an argument-scoped rule
// (`Bash(git *)` matches `git status; rm -rf /`). Argument globs can
// only be trusted against a single simple command, so callers matching
// rules against shell commands must treat a true result as unmatched
// (deny, or escalate to approval). The check is intentionally
// conservative: a plain substring scan, so an operator inside quotes is
// also flagged — for a security gate, over-rejecting a quoted `;` beats
// parsing shell grammar and risking a miss.
func HasShellControl(cmd string) bool {
	return strings.ContainsAny(cmd, ";|&\n`") || strings.Contains(cmd, "$(")
}

// matches reports whether the rule applies to an invocation of tool with
// the given argument string.
func (r compiledRule) matches(tool, arg string) bool {
	if !strings.EqualFold(r.Tool, tool) {
		return false
	}
	if r.Arg == "" {
		return true
	}
	return r.re != nil && r.re.MatchString(arg)
}

// globRegexp compiles a glob into an anchored regexp. `*` matches any
// run of characters (including separators), `?` matches exactly one. All
// other characters are literal. Unlike path.Match, `*` crosses `/` — a
// bash command grant like `Bash(git *)` must match commands with paths.
func globRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString(`\A`)
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(`.*`)
		case '?':
			b.WriteString(`.`)
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString(`\z`)
	return regexp.Compile(b.String())
}

// joinErrs wraps rule-parse errors with an "N invalid rule(s)" frame
// while keeping each cause reachable via errors.Is/As.
func joinErrs(errs []error) error {
	return fmt.Errorf("toolperm: %d invalid rule(s): %w", len(errs), errors.Join(errs...))
}
