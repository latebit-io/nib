package kit

// PromptContributor is the optional interface a [Tool] may implement to
// contribute usage guidance to a consumer's system prompt. The bullets
// describe non-obvious behaviors and anti-patterns the tool's schema
// alone cannot teach ("keep search short", "prefer this tool over bash
// for X") — the schema advertises WHAT a tool does; guidelines teach
// HOW to use it well.
//
// The agent core never reads this interface: prompt construction is an
// application-layer concern, so consumers (the coding agent's prompt
// builder, any future kit-built binary) collect guidance themselves via
// [ToolPromptGuidelines]. A tool that implements PromptContributor is
// self-documenting at the prompt level — registering it carries its
// guidance with it, with no central template edit. This mirrors the
// [Described] pattern for the introspection surface.
type PromptContributor interface {
	// PromptGuidelines returns guidance bullets for the system prompt.
	// Each entry is one bullet: a single sentence or two, no leading
	// dash, no trailing newline. The returned slice must be stable
	// across calls. A tool whose guidance depends on registration-time
	// wiring may compute the slice at construction; it must not vary
	// per invocation.
	PromptGuidelines() []string
}

// ToolPromptGuidelines returns t's prompt guidance bullets, or nil when
// the concrete type does not implement [PromptContributor]. Consumers
// building a system prompt call this per registered tool, preserving
// registration order so the rendered section is stable across runs.
func ToolPromptGuidelines(t Tool) []string {
	if p, ok := t.(PromptContributor); ok {
		return p.PromptGuidelines()
	}
	return nil
}
