package contracttest

import (
	"testing"

	"github.com/latebit-io/nib/engine/capture"
)

// TestSessionEventSink_NoopSinkPasses confirms the NoopSink — the
// canonical conforming implementation in the engine — passes the
// fixture. Doubles as a sanity check that the fixture itself is
// wired correctly.
func TestSessionEventSink_NoopSinkPasses(t *testing.T) {
	SessionEventSink(t, func() capture.SessionEventSink {
		return capture.NoopSink{}
	})
}
