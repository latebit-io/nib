package kit

import "testing"

// mockLifecycle is a trivial fake that records Close invocations.
type mockLifecycle struct {
	closes int
}

// Close satisfies [AgentLifecycle].
func (m *mockLifecycle) Close() { m.closes++ }

// TestAgentLifecycleInterface verifies the contract surface is callable
// through the interface type. The compile-time assertions in
// lifecycle.go cover *Agent; this test exercises an arbitrary
// implementation to confirm the port is usable from outside the kit
// package's own concrete type.
func TestAgentLifecycleInterface(t *testing.T) {
	var lc AgentLifecycle = &mockLifecycle{}
	lc.Close()
	lc.Close()

	got := lc.(*mockLifecycle).closes
	if got != 2 {
		t.Errorf("Close calls = %d, want 2", got)
	}
}
