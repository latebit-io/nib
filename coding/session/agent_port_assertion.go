package session

import "github.com/latebit-io/nib/coding/agent"

// Compile-time guarantee that *agent.Agent satisfies the local agentPort
// interface. Without this, a method rename on *agent.Agent would only
// fail at the (one) construction site rather than at every consumer of
// the interface — and the headless package's analogous interface would
// drift independently. The assertion is the cheap safety net that flags
// the divergence at build time.
var _ agentPort = (*agent.Agent)(nil)
