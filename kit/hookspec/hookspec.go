package hookspec

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Event is a lifecycle point at which hooks fire. The constants are the
// Claude Code event names nib recognizes; an unrecognized event in a
// hooks file is preserved (so a converter can report it) but never
// emitted by the engine.
type Event string

const (
	// PreToolUse fires before a tool call; a command hook may deny it.
	PreToolUse Event = "PreToolUse"
	// PostToolUse fires after a tool call completes.
	PostToolUse Event = "PostToolUse"
	// UserPromptSubmit fires when the user submits a prompt.
	UserPromptSubmit Event = "UserPromptSubmit"
	// SessionStart fires once when a session begins.
	SessionStart Event = "SessionStart"
	// Stop fires when the agent finishes a run / yields.
	Stop Event = "Stop"
	// SubagentStop fires when a spawned subagent finishes.
	SubagentStop Event = "SubagentStop"
	// PreCompact fires before conversation history is compacted.
	PreCompact Event = "PreCompact"
	// SessionEnd fires once when a session ends.
	SessionEnd Event = "SessionEnd"
)

// known is the set of events the engine can emit. Others parse fine but
// are reported as unsupported by consumers.
var known = map[Event]bool{
	PreToolUse: true, PostToolUse: true, UserPromptSubmit: true,
	SessionStart: true, Stop: true, SubagentStop: true, PreCompact: true, SessionEnd: true,
}

// Known reports whether the engine recognizes (and can emit) the event.
func (e Event) Known() bool { return known[e] }

// HookType is the kind of action a hook performs.
type HookType string

const (
	// Command runs a shell command (JSON stdin → decision stdout). The
	// only type nib runs in v1.
	Command HookType = "command"
	// HTTP posts the event to a URL. Parsed, not yet run.
	HTTP HookType = "http"
	// MCPTool calls a configured MCP tool. Parsed, not yet run.
	MCPTool HookType = "mcp_tool"
	// Prompt asks the LLM to evaluate. Parsed, not yet run.
	Prompt HookType = "prompt"
	// Agent runs an agentic verifier. Parsed, not yet run.
	Agent HookType = "agent"
)

// Hook is one action to run when its group's event fires and matcher
// matches. Only the fields relevant to Type are meaningful.
type Hook struct {
	// Type selects the action kind.
	Type HookType `json:"type"`
	// Command is the shell command for [Command] hooks. It preserves the
	// authored form — a bare string runs via the shell, an argv array runs
	// without one (see [CommandSpec]).
	Command CommandSpec `json:"command"`
	// Args, when set, run Command as a program with these arguments and
	// no shell (Claude Code's exec form).
	Args []string `json:"args,omitempty"`
	// Env are extra environment variables for a command hook.
	Env map[string]string `json:"env,omitempty"`
	// Timeout bounds a command/http hook, in seconds as in Claude Code
	// (0 = caller default).
	Timeout int `json:"timeout,omitempty"`
	// URL is the endpoint for [HTTP] hooks.
	URL string `json:"url,omitempty"`
	// Server / Tool name an [MCPTool] hook.
	Server string `json:"server,omitempty"`
	Tool   string `json:"tool,omitempty"`
	// Prompt is the instruction for [Prompt] hooks.
	Prompt string `json:"prompt,omitempty"`
	// AgentName is the verifier for [Agent] hooks.
	AgentName string `json:"agent,omitempty"`
}

// Runnable reports whether nib can execute this hook today — a command
// hook with a non-blank command. Other types parse but are inert until
// their runner lands.
func (h Hook) Runnable() bool { return h.Type == Command && !h.Command.Blank() }

// CommandSpec is a hook command that remembers whether it was authored as
// a bare shell string or an argv array, so the runner can run the former
// via the shell and the latter directly, and so the form round-trips
// through [Config.Marshal] (the importer writes a managed copy).
type CommandSpec struct {
	// Parts is the command: a single shell string (Shell true) or argv
	// tokens (Shell false).
	Parts []string
	// Shell reports whether Parts is a single shell string.
	Shell bool
}

// UnmarshalJSON accepts a string (shell form) or a string array (argv).
func (c *CommandSpec) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		c.Parts, c.Shell = []string{s}, true
		return nil
	}
	var a []string
	if err := json.Unmarshal(b, &a); err != nil {
		return fmt.Errorf("command must be a string or string array: %w", err)
	}
	c.Parts, c.Shell = a, false
	return nil
}

// Blank reports whether the command is effectively empty — no parts, or
// a first part (the shell string, or the program for argv) that is blank.
// A blank command would be a silent no-op, so it is rejected at parse and
// is never [Hook.Runnable].
func (c CommandSpec) Blank() bool {
	return len(c.Parts) == 0 || strings.TrimSpace(c.Parts[0]) == ""
}

// MarshalJSON re-emits the authored form: a string for a shell command,
// an array otherwise.
func (c CommandSpec) MarshalJSON() ([]byte, error) {
	if c.Shell && len(c.Parts) == 1 {
		return json.Marshal(c.Parts[0])
	}
	return json.Marshal(c.Parts)
}

// Group binds a tool-name matcher to a set of hooks under one event.
type Group struct {
	// Matcher selects tools as Claude Code does: empty or "*" matches
	// every tool; names (letters, digits, _ - and spaces) separated by |
	// or , match exactly; anything else is an unanchored regex.
	Matcher string `json:"matcher"`
	// Hooks run when the event fires and Matcher matches.
	Hooks []Hook `json:"hooks"`

	re    *regexp.Regexp // compiled Matcher; nil ⇒ compile lazily
	names []string       // a matcher of names, split by compile
}

// matcherCache memoizes lazily compiled matchers for Groups built by
// literal or json.Unmarshal (which bypass [Parse]), so Matches stays
// cheap without a shared mutable Group.
var matcherCache sync.Map // pattern → *regexp.Regexp

// Matches reports whether the group applies to a call of the named tool
// (see [Group.Matcher]). A Group not produced by [Parse] compiles its
// matcher on first use; an invalid pattern never matches.
func (g Group) Matches(tool string) bool {
	if g.Matcher == "" || g.Matcher == "*" {
		return true
	}
	if g.names != nil {
		return slices.Contains(g.names, tool)
	}
	if names, ok := nameList(g.Matcher); ok {
		return slices.Contains(names, tool)
	}
	re := g.re
	if re == nil {
		if cached, ok := matcherCache.Load(g.Matcher); ok {
			re = cached.(*regexp.Regexp)
		} else {
			var err error
			if re, err = regexp.Compile(g.Matcher); err != nil {
				return false
			}
			matcherCache.Store(g.Matcher, re)
		}
	}
	return re.MatchString(tool)
}

// nameList splits a matcher made only of names into them; ok is false
// for a matcher that is a regex.
func nameList(matcher string) (names []string, ok bool) {
	for _, c := range matcher {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '-' || c == ' ' || c == ',' || c == '|':
		default:
			return nil, false
		}
	}
	for name := range strings.FieldsFuncSeq(matcher, func(c rune) bool { return c == '|' || c == ',' }) {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names, true
}

// compile builds the matcher regex. A matcher of names, or one that
// matches everything, needs none; an invalid regex is an error.
func (g *Group) compile() error {
	if g.Matcher == "" || g.Matcher == "*" {
		return nil
	}
	if names, ok := nameList(g.Matcher); ok {
		g.names = names
		return nil
	}
	re, err := regexp.Compile(g.Matcher)
	if err != nil {
		return fmt.Errorf("invalid matcher %q: %w", g.Matcher, err)
	}
	g.re = re
	return nil
}

// Config is a parsed hooks file: events mapped to their groups.
type Config struct {
	// Hooks maps each configured event to its groups.
	Hooks map[Event][]Group
}

// Marshal renders the config back to the on-disk `{"hooks": {...}}` shape
// (indented). Command fields normalize to arrays and the compiled matcher
// is not serialized. Used by the importer to write the managed copy.
func (c Config) Marshal() ([]byte, error) {
	return json.MarshalIndent(hooksFile(c), "", "  ")
}

// Unsupported describes a parsed-but-not-yet-runnable element, for a
// converter/loader to report rather than silently drop.
type Unsupported struct {
	// Event is where the element appeared.
	Event Event
	// Reason explains why it cannot run yet.
	Reason string
}

// Unsupported returns the elements nib parsed but cannot honor yet:
// unknown events and non-command hook types. Command hooks under known
// events are omitted (they are runnable).
func (c Config) Unsupported() []Unsupported {
	var out []Unsupported
	for ev, groups := range c.Hooks {
		if !ev.Known() {
			out = append(out, Unsupported{Event: ev, Reason: "unrecognized event"})
		}
		for _, g := range groups {
			for _, h := range g.Hooks {
				if h.Type != Command {
					out = append(out, Unsupported{Event: ev, Reason: fmt.Sprintf("%q hook type not yet runnable", h.Type)})
				}
			}
		}
	}
	return out
}

// hooksFile is the on-disk top-level shape: {"hooks": {<event>: [...]}}.
type hooksFile struct {
	Hooks map[Event][]Group `json:"hooks"`
}

// Parse decodes a hooks.json body and compiles every group's matcher. A
// matcher given as an object (CC's `{ "if": [...] }` form) is rejected:
// JSON cannot unmarshal it into the string matcher field, surfacing a
// clear error rather than a silently-ignored condition.
func Parse(data []byte) (Config, error) {
	var f hooksFile
	if err := json.Unmarshal(data, &f); err != nil {
		return Config{}, fmt.Errorf("hookspec: parse hooks: %w", err)
	}
	for ev, groups := range f.Hooks {
		for i := range groups {
			if err := groups[i].compile(); err != nil {
				return Config{}, fmt.Errorf("hookspec: event %s: %w", ev, err)
			}
			for _, h := range groups[i].Hooks {
				// A command hook with a blank command (missing, empty array,
				// or empty string) would silently no-op at runtime — reject
				// it so a broken hook fails import instead of disappearing.
				if h.Type == Command && h.Command.Blank() {
					return Config{}, fmt.Errorf("hookspec: event %s: command hook has an empty command", ev)
				}
			}
		}
		f.Hooks[ev] = groups
	}
	return Config(f), nil
}
