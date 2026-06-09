// Package memory holds the agent-side memory-integration primitives.
//
// Two concerns live here today: distributed-memory detection (classifying
// MCP servers as shared/team by naming convention) and a thin helper for
// re-fetching the project summary at run time. Both are pure functions —
// no agent-state coupling — so they are testable without standing up an
// Agent.
//
// The underlying memory store (document fetch/publish) is provided by
// [github.com/latebit-io/nib/kit/memory/demarkus]. This package adapts the
// store for the agent's prompting and run-lifecycle needs; it does NOT
// own the LLM-facing memory tools (those live in [coding/tools]).
package memory

import "strings"

// distributedKeywords are substrings that identify an MCP server as
// distributed (team/shared) memory. If any keyword appears in the server
// name (case-insensitive), the server is classified as distributed memory
// and the system prompt explains local vs shared usage to the LLM.
var distributedKeywords = []string{"team", "shared", "distributed", "soul"}

// DetectDistributedMemory filters server names by naming convention,
// returning those that indicate a shared/team memory server.
func DetectDistributedMemory(serverNames []string) []string {
	var result []string
	for _, name := range serverNames {
		lower := strings.ToLower(name)
		for _, kw := range distributedKeywords {
			if strings.Contains(lower, kw) {
				result = append(result, name)
				break
			}
		}
	}
	return result
}
