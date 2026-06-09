package tools

import "github.com/latebit-io/nib/agent"

// Tool aliases the upstream generic [agent.Tool] interface so each
// tool in this package can name it unqualified. Tools satisfy the
// interface implicitly via Definition / Execute methods.
type Tool = agent.Tool

// ToolResult aliases the upstream generic [agent.ToolResult]. The
// shape is deliberately minimal: a string body and an error flag.
type ToolResult = agent.ToolResult

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
	Reset()
}
