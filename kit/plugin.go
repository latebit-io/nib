package kit

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/latebit-io/nib/kit/command"
)

// Plugin is the descriptive surface for any kit-composable artifact —
// tool, slash command, provider, or memory store. It drives `--plugins`
// CLI introspection, structured diagnostics, and (eventually) /help
// auto-generation.
//
// Plugin is a value type. Construct directly or derive from an existing
// port method via [DescribeTool] / [DescribeCommand]. A concrete
// implementation that wants to override default derivation implements
// the optional [Described] interface.
//
// Concrete use cases that motivated the type:
//
//   - `nib-code --plugins` prints the active wiring (provider, store,
//     tools, commands) so a daily-driver user can confirm which
//     markdown commands loaded and which MCP tools are exposed without
//     consulting docs that drift.
//   - `nibster --plugins` prints the small kit-only surface as a smoke
//     test that the kit boundary still holds.
//   - A third-party `Plugins() kit.Toolset` author runs `--plugins`
//     after wiring their package into a binary to verify their tools
//     and commands flowed through.
//   - Bug reports paste `--plugins` output so the maintainer sees the
//     active setup without follow-up questions.
type Plugin struct {
	// Kind classifies the artifact for grouped rendering.
	Kind PluginKind

	// Name is the canonical short identifier (tool function name,
	// command name without leading slash, provider profile name,
	// store identifier). Stable across versions.
	Name string

	// Description is the one-line summary surfaced verbatim by
	// renderers. Derived from the artifact's existing port method
	// when not overridden via [Described].
	Description string

	// Version is the artifact's version, when known. Empty for
	// built-ins (the binary's own version is the relevant marker
	// for the whole bundle). External plug-in packages set this
	// to a semver string via [Described.Describe].
	Version string

	// Source is a free-form origin pointer. Examples: "builtin",
	// "mcp:<server>", ".project/commands/<file>.md", "module:foo/bar".
	// Renderers display when non-empty.
	Source string
}

// PluginKind classifies a [Plugin] for grouped rendering. The renderer
// orders groups: provider, store, tool, command.
type PluginKind string

const (
	// KindTool tags an artifact backing the kit.Tool port.
	KindTool PluginKind = "tool"
	// KindCommand tags an artifact backing the command.Command port.
	KindCommand PluginKind = "command"
	// KindProvider tags an LLM provider (binary supplies the
	// metadata; the [llm.Provider] interface has no Describe method
	// and cannot, since ai/llm cannot depend on kit).
	KindProvider PluginKind = "provider"
	// KindStore tags a memory.Store (binary supplies the metadata
	// for the same layering reason as KindProvider).
	KindStore PluginKind = "store"
)

// Described is the optional interface a kit-composable artifact may
// implement to override default metadata derivation. When a concrete
// type implements Described, [DescribeTool] / [DescribeCommand] return
// the Describe() result verbatim. Otherwise the helpers derive the
// Plugin from the artifact's existing port method.
//
// External plug-in packages typically implement Described to surface
// a semver Version. Built-in tools and commands do not implement it
// — their Plugin metadata is derivable from their port methods alone.
type Described interface {
	// Describe returns the Plugin metadata for this artifact. The
	// returned value must be stable across calls.
	Describe() Plugin
}

// DescribeTool returns the [Plugin] metadata for a [Tool]. If the
// concrete type implements [Described], its Describe() result wins
// (with Kind forced to [KindTool] to prevent miscategorization).
// Otherwise the Plugin is derived from t.Definition().Function —
// Name and Description map directly; Source defaults to "builtin".
//
// Tool descriptions are typically LLM-facing prompts spanning several
// sentences. The Description field is truncated to the first sentence
// (split at the first period followed by space/end) for the human
// listing surface; the full text remains available to callers that
// inspect Tool.Definition() directly.
func DescribeTool(t Tool) Plugin {
	if d, ok := t.(Described); ok {
		p := d.Describe()
		p.Kind = KindTool
		return p
	}
	def := t.Definition().Function
	return Plugin{
		Kind:        KindTool,
		Name:        def.Name,
		Description: firstSentence(def.Description),
		Source:      "builtin",
	}
}

// firstSentence returns the leading sentence of s — text up to the
// first period followed by a space or end of string. If no terminating
// period exists, the whole string is returned. Tool definitions
// frequently begin with a one-line summary followed by detailed
// LLM-facing instructions; this surface produces the summary alone
// for human-readable listings.
func firstSentence(s string) string {
	rest := s
	for {
		i := strings.IndexByte(rest, '.')
		if i < 0 {
			return s
		}
		end := len(s) - len(rest) + i + 1
		if end == len(s) || strings.IndexByte(" \n\t", s[end]) >= 0 {
			return s[:end]
		}
		rest = rest[i+1:]
	}
}

// DescribeCommand returns the [Plugin] metadata for a slash
// [command.Command]. Described override path identical to
// [DescribeTool]; default derivation pulls Name, Description from
// the command's Definition() and maps Source from Definition().Source
// (kind + path).
func DescribeCommand(c command.Command) Plugin {
	if d, ok := c.(Described); ok {
		p := d.Describe()
		p.Kind = KindCommand
		return p
	}
	def := c.Definition()
	return Plugin{
		Kind:        KindCommand,
		Name:        def.Name,
		Description: def.Description,
		Source:      commandSource(def.Source),
	}
}

// commandSource renders a [command.Source] as the free-form Source
// string used in Plugin output. Kind is the broad category; Path is
// included when present.
func commandSource(s command.Source) string {
	kind := s.Kind.String()
	if s.Path == "" {
		return kind
	}
	return kind + ":" + s.Path
}

// RenderPlugins returns a grouped, human-readable summary of plugins.
// Output is stable and line-parseable; each line within a group is
// "  <name>  <description>" with two-space indent. Groups are emitted
// in the order Provider, Store, Tool, Command, separated by a blank
// line. Within a group, entries sort by Name. Empty groups are
// omitted. Caller writes the returned string to its preferred sink so
// I/O error handling stays at the boundary.
//
// Format example:
//
//	PROVIDER (1)
//	  openrouter           google/gemini-2.5-flash
//
//	TOOL (3)
//	  bash                 Run shell commands with output cap + timeout
//	  memory_fetch         Fetch a versioned memory document
//	  search               Project search (engine-backed)
//
// The renderer pads names to a per-group width so descriptions
// align. Version and Source are appended in brackets when present:
// "  foo@1.2.0          Description [module:foo/bar]".
func RenderPlugins(plugins []Plugin) string {
	var b strings.Builder
	groups := groupPlugins(plugins)

	first := true
	for _, kind := range pluginKindOrder {
		group := groups[kind]
		if len(group) == 0 {
			continue
		}
		if !first {
			b.WriteByte('\n')
		}
		first = false

		fmt.Fprintf(&b, "%s (%d)\n", strings.ToUpper(string(kind)), len(group))

		nameWidth := groupNameWidth(group)
		for _, p := range group {
			fmt.Fprintf(&b, "  %-*s  %s", nameWidth, displayName(p), p.Description)
			if p.Source != "" && p.Source != "builtin" {
				fmt.Fprintf(&b, " [%s]", p.Source)
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// pluginKindOrder is the stable group emission order used by
// [RenderPlugins]. Provider first (the most user-visible identity),
// then Store (durable state), Tool, and finally Command (the
// largest and least operational group).
var pluginKindOrder = []PluginKind{
	KindProvider,
	KindStore,
	KindTool,
	KindCommand,
}

// groupPlugins partitions plugins by Kind and sorts each group by
// Name. The returned map is keyed by Kind; entries are sorted
// alphabetically within each group for deterministic output.
func groupPlugins(plugins []Plugin) map[PluginKind][]Plugin {
	groups := make(map[PluginKind][]Plugin, len(pluginKindOrder))
	for _, p := range plugins {
		groups[p.Kind] = append(groups[p.Kind], p)
	}
	for kind := range groups {
		g := groups[kind]
		slices.SortFunc(g, func(a, b Plugin) int { return strings.Compare(a.Name, b.Name) })
		groups[kind] = g
	}
	return groups
}

// groupNameWidth returns the column width used for name padding in a
// single group. Width is the max of all display names in the group,
// capped at 24 so an unusually long plug-in name does not push every
// description off the next column. Names longer than the cap render
// without alignment.
func groupNameWidth(group []Plugin) int {
	const maxNameWidth = 24
	width := 0
	for _, p := range group {
		// Rune count, matching how fmt's %-*s pads.
		width = max(width, utf8.RuneCountInString(displayName(p)))
	}
	if width > maxNameWidth {
		return maxNameWidth
	}
	return width
}

// displayName renders the Name@Version form when Version is
// non-empty; otherwise just Name. Commands get a leading slash so
// "/help" is recognizable in --plugins output.
func displayName(p Plugin) string {
	name := p.Name
	if p.Kind == KindCommand && !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	if p.Version != "" {
		name += "@" + p.Version
	}
	return name
}
