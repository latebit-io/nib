package agent

import "github.com/latebit-io/nib/coding/memory"

// DetectDistributedMemory is an alias of [memory.DetectDistributedMemory]
// kept here so callers (cmd/agent, tui/cmd/tui) that import the agent
// package for this helper continue to work without immediate import
// changes. Phase 7 of the layering refactor strips the alias and migrates
// callers to the canonical import.
var DetectDistributedMemory = memory.DetectDistributedMemory
