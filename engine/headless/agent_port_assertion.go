package headless

import "github.com/latebit-io/junto/engine/agent"

// Compile-time guarantee that *agent.Agent satisfies the local agentPort
// interface. The session and headless packages each depend on a
// different narrow subset of *agent.Agent's surface; without explicit
// assertions in both packages, a method rename only fails at one
// construction site and the divergence is invisible until runtime. The
// assertion is the cheap safety net that flags the drift at build time.
var _ agentPort = (*agent.Agent)(nil)
