package hookspec

import (
	"encoding/json"
	"fmt"
	"regexp"
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
)

// known is the set of events the engine can emit. Others parse fine but
// are reported as unsupported by consumers.
var known = map[Event]bool{
	PreToolUse: true, PostToolUse: true, UserPromptSubmit: true,
	SessionStart: true, Stop: true, SubagentStop: true, PreCompact: true,
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
	// Command is the shell command for [Command] hooks, normalized to a
	// slice (CC permits a bare string or an argv array).
	Command jsonStrings `json:"command"`
	// Env are extra environment variables for a command hook.
	Env map[string]string `json:"env,omitempty"`
	// TimeoutMS bounds a command/http hook (0 = caller default).
	TimeoutMS int `json:"timeout,omitempty"`
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
// hook with a non-empty command. Other types parse but are inert until
// their runner lands.
func (h Hook) Runnable() bool { return h.Type == Command && len(h.Command) > 0 }

// Group binds a tool-name matcher to a set of hooks under one event.
type Group struct {
	// Matcher is a regular expression tested against the tool name. An
	// empty matcher matches every tool (and non-tool events, where the
	// matcher is irrelevant).
	Matcher string `json:"matcher"`
	// Hooks run when the event fires and Matcher matches.
	Hooks []Hook `json:"hooks"`

	re *regexp.Regexp // compiled Matcher; nil ⇒ match all
}

// Matches reports whether the group applies to a call of the named tool.
// An empty matcher always matches; otherwise the matcher regex is tested
// (unanchored) against the tool name.
func (g Group) Matches(tool string) bool {
	if g.re == nil {
		return true
	}
	return g.re.MatchString(tool)
}

// compile builds the matcher regex. A blank matcher leaves re nil
// (match-all); an invalid regex is an error.
func (g *Group) compile() error {
	if g.Matcher == "" {
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
		}
		f.Hooks[ev] = groups
	}
	return Config(f), nil
}

// jsonStrings unmarshals a JSON field that may be a single string or an
// array of strings into a []string (CC allows a command as either form).
type jsonStrings []string

// UnmarshalJSON implements [json.Unmarshaler].
func (j *jsonStrings) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*j = jsonStrings{s}
		return nil
	}
	var a []string
	if err := json.Unmarshal(b, &a); err != nil {
		return fmt.Errorf("expected string or string array: %w", err)
	}
	*j = a
	return nil
}
