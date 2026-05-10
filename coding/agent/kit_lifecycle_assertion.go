package agent

import "github.com/latebit-io/nib/kit"

// Compile-time guarantee that *Agent satisfies [kit.AgentLifecycle]. Any
// change to Close that breaks the kit-side contract fails the build at
// the contract definition rather than at the (one) frontend site that
// holds the port.
var _ kit.AgentLifecycle = (*Agent)(nil)
