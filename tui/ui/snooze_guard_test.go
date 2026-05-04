package ui

import "testing"

// TestReconcilePendingBlockOnOutcome locks in the snooze invariant:
// blockedPaths gains an entry IFF the apply pipeline returned
// outcomeSucceeded. Both terminal failure paths drop the pending
// snapshot; the retryable failure leaves it intact so a follow-up
// approve still snoozes.
//
// The original bug this test guards against: blockedPaths was
// mutated at the Ctrl+O keypress, before PrepareApproval/ApplyEdit
// ran. A failure in either step left the path silently snoozed for
// the rest of the session, so the next Block on that file
// auto-applied without developer review.
//
// Tested at the helper level (not via the full applyApproval flow)
// because the regression is a state-ordering bug, not a pipeline
// integration bug. Forcing the test to drive Session+Editor would
// add hundreds of lines of setup without exercising any logic the
// helper doesn't already cover.
func TestReconcilePendingBlockOnOutcome(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name              string
		seedPending       string
		seedBlocked       map[string]bool
		blockedPathArg    string
		outcome           approvalOutcome
		wantPendingAfter  string
		wantBlockedAfter  map[string]bool
		wantPathInBlocked string // non-empty: assert this key set true
	}{
		{
			name:              "success with pending block promotes to snooze",
			seedPending:       "src/main.lua",
			seedBlocked:       map[string]bool{},
			blockedPathArg:    "src/main.lua",
			outcome:           outcomeSucceeded,
			wantPendingAfter:  "",
			wantPathInBlocked: "src/main.lua",
		},
		{
			name:             "success without pending block is a no-op on snooze map",
			seedPending:      "",
			seedBlocked:      map[string]bool{},
			blockedPathArg:   "",
			outcome:          outcomeSucceeded,
			wantPendingAfter: "",
			wantBlockedAfter: map[string]bool{},
		},
		{
			name:             "preparation-failed terminal drops pending without snoozing",
			seedPending:      "src/main.lua",
			seedBlocked:      map[string]bool{},
			blockedPathArg:   "src/main.lua",
			outcome:          outcomePreparationFailedTerminal,
			wantPendingAfter: "",
			wantBlockedAfter: map[string]bool{},
		},
		{
			name:             "preparation-failed retryable keeps pending so retry can still snooze",
			seedPending:      "src/main.lua",
			seedBlocked:      map[string]bool{},
			blockedPathArg:   "src/main.lua",
			outcome:          outcomePreparationFailedRetryable,
			wantPendingAfter: "src/main.lua",
			wantBlockedAfter: map[string]bool{},
		},
		{
			name:             "apply-failed drops pending without snoozing — the original regression",
			seedPending:      "src/main.lua",
			seedBlocked:      map[string]bool{},
			blockedPathArg:   "src/main.lua",
			outcome:          outcomeApplyFailed,
			wantPendingAfter: "",
			wantBlockedAfter: map[string]bool{},
		},
		{
			name:              "success preserves pre-existing snoozes",
			seedPending:       "src/new.lua",
			seedBlocked:       map[string]bool{"src/old.lua": true},
			blockedPathArg:    "src/new.lua",
			outcome:           outcomeSucceeded,
			wantPendingAfter:  "",
			wantPathInBlocked: "src/new.lua",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &AppModel{
				blockedPaths:       cloneBoolMap(tc.seedBlocked),
				pendingBlockedPath: tc.seedPending,
			}
			m.reconcilePendingBlockOnOutcome(tc.blockedPathArg, tc.outcome)

			if m.pendingBlockedPath != tc.wantPendingAfter {
				t.Errorf("pendingBlockedPath = %q, want %q",
					m.pendingBlockedPath, tc.wantPendingAfter)
			}
			if tc.wantPathInBlocked != "" {
				if !m.blockedPaths[tc.wantPathInBlocked] {
					t.Errorf("blockedPaths[%q] = false, want true (snooze must land on success)",
						tc.wantPathInBlocked)
				}
				return
			}
			if !boolMapsEqual(m.blockedPaths, tc.wantBlockedAfter) {
				t.Errorf("blockedPaths = %v, want %v", m.blockedPaths, tc.wantBlockedAfter)
			}
		})
	}
}

// TestReconcilePendingBlockOnOutcome_OldSnoozesUntouchedOnFailure
// pins the no-op-on-failure invariant: a previously snoozed path
// must NOT be cleared when a different file's approval fails. Without
// this, an approval failure on file B could plausibly be coded to
// "reset all snoozes," undoing earlier eyes-on decisions.
func TestReconcilePendingBlockOnOutcome_OldSnoozesUntouchedOnFailure(t *testing.T) {
	t.Parallel()

	m := &AppModel{
		blockedPaths:       map[string]bool{"src/already-approved.lua": true},
		pendingBlockedPath: "src/new.lua",
	}
	m.reconcilePendingBlockOnOutcome("src/new.lua", outcomeApplyFailed)

	if !m.blockedPaths["src/already-approved.lua"] {
		t.Errorf("pre-existing snooze cleared by unrelated apply failure: %v", m.blockedPaths)
	}
	if m.blockedPaths["src/new.lua"] {
		t.Errorf("apply-failed path was incorrectly snoozed: %v", m.blockedPaths)
	}
	if m.pendingBlockedPath != "" {
		t.Errorf("pendingBlockedPath = %q, want empty after terminal failure", m.pendingBlockedPath)
	}
}

func cloneBoolMap(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func boolMapsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
