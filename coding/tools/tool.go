package tools

import (
	"maps"
	"strings"

	"github.com/latebit-io/nib/kit"
)

// Tool aliases the [kit.Tool] port so each tool in this package can
// name it unqualified. Tools satisfy the interface implicitly via
// Definition / Execute methods.
type Tool = kit.Tool

// ToolResult aliases [kit.ToolResult]. The shape is deliberately
// minimal: a string body and an error flag.
type ToolResult = kit.ToolResult

// textResult builds a successful tool result whose body is the given
// string. Convenience constructor used by every tool.
func textResult(content string) ToolResult {
	return ToolResult{Content: content}
}

// errorResult builds a tool result whose body is the given error
// message and whose IsError flag is set so frontends can render it
// distinctively.
func errorResult(content string) ToolResult {
	return ToolResult{Content: content, IsError: true}
}

// Resettable is optionally implemented by tools that carry state
// between calls (e.g. retry counters). The agent calls Reset on each
// new run.
type Resettable interface {
	// Reset clears per-run state; called at the start of every run and
	// must be safe while a cancelled Execute is still unwinding.
	Reset()
}

// mutatingToolNames is the canonical set of tool names that modify
// filesystem or shell state. Single source for the agent's active-task
// gate / lifecycle schema and for the planning-mode blocklist, so the
// two can never disagree. Unexported: exporting a map exports a
// mutation surface — go through [IsMutatingTool] / [MutatingToolNames].
var mutatingToolNames = map[string]bool{
	"edit_file":    true,
	"write_file":   true,
	"replace_file": true,
	"apply_patch":  true,
	"bash":         true,
	"smoke_run":    true,
}

// IsMutatingTool reports whether the (lower-cased) tool name modifies
// filesystem or shell state.
func IsMutatingTool(name string) bool {
	return mutatingToolNames[strings.ToLower(name)]
}

// MutatingToolNames returns a fresh copy of the mutating-tool set that
// callers may extend without affecting the package-level defaults.
func MutatingToolNames() map[string]bool {
	return maps.Clone(mutatingToolNames)
}
