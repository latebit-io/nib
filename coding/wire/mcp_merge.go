package wire

import "github.com/latebit-io/nib/coding/agent"

// Merge returns a new MCPResult combining r and other: tools and server
// names concatenated (r first), cleanups chained so both client sets
// close on shutdown. Neither operand is mutated. Used to fold
// plugin-contributed MCP servers into the project's discovery result.
func (r MCPResult) Merge(other MCPResult) MCPResult {
	return MCPResult{
		Tools:       append(append([]agent.Tool{}, r.Tools...), other.Tools...),
		ServerNames: append(append([]string{}, r.ServerNames...), other.ServerNames...),
		Cleanup: func() {
			if r.Cleanup != nil {
				r.Cleanup()
			}
			if other.Cleanup != nil {
				other.Cleanup()
			}
		},
	}
}
