package session

import "testing"

// TestAutonomyLevelCycle verifies the dial wraps from LevelYolo back
// to LevelGuided.
func TestAutonomyLevelCycle(t *testing.T) {
	t.Parallel()

	cases := []struct {
		from, to AutonomyLevel
	}{
		{LevelGuided, LevelTrusted},
		{LevelTrusted, LevelYolo},
		{LevelYolo, LevelGuided},
	}
	for _, tc := range cases {
		if got := tc.from.Cycle(); got != tc.to {
			t.Errorf("Cycle(%s) = %s, want %s", tc.from.Name(), got.Name(), tc.to.Name())
		}
	}
}

// TestAutonomyLevelFlags verifies the boolean predicates each level
// produces. Tabular so adding a level forces an explicit row — a
// future maintainer adding LevelX has to think about each axis.
func TestAutonomyLevelFlags(t *testing.T) {
	t.Parallel()

	cases := []struct {
		level        AutonomyLevel
		approveEdits bool
		approveBlock bool
	}{
		{LevelGuided, false, false},
		{LevelTrusted, true, false},
		{LevelYolo, true, true},
	}
	for _, tc := range cases {
		if got := tc.level.AutoApproveEdits(); got != tc.approveEdits {
			t.Errorf("%s.AutoApproveEdits() = %v, want %v", tc.level.Name(), got, tc.approveEdits)
		}
		if got := tc.level.AutoApproveBlock(); got != tc.approveBlock {
			t.Errorf("%s.AutoApproveBlock() = %v, want %v", tc.level.Name(), got, tc.approveBlock)
		}
	}
}

// TestAutonomyLevelNames locks the human-facing labels so a UI
// change has to go through the test diff.
func TestAutonomyLevelNames(t *testing.T) {
	t.Parallel()

	cases := []struct {
		level AutonomyLevel
		name  string
	}{
		{LevelGuided, "guided"},
		{LevelTrusted, "trust"},
		{LevelYolo, "yolo"},
	}
	for _, tc := range cases {
		if got := tc.level.Name(); got != tc.name {
			t.Errorf("Name() for level %d = %q, want %q", int(tc.level), got, tc.name)
		}
	}
}
