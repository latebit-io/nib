package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// tab simulates a Tab keypress (with optional Shift) on the agent pane
// without going through the textarea. Tests should call this on a pane
// whose input is NOT active, which mirrors the developer's flow:
// Esc out of input, Tab to navigate beats.
func tab(m *AgentPaneModel, shift bool) {
	mod := tea.KeyMod(0)
	if shift {
		mod |= tea.ModShift
	}
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyTab, Mod: mod})
}

// TestTab_TogglesActiveBeatCollapse verifies plain Tab on the active
// beat flips its collapse flag and stamps UserOverride so the auto
// policy can't undo it on the next transition.
func TestTab_TogglesActiveBeatCollapse(t *testing.T) {
	m := beatPane()
	m.AppendUserMessage("hello")
	m.AppendToken("agent reply\n")

	// Active beat is the last one, expanded by default.
	active := len(m.beats) - 1
	if m.beats[active].Collapsed {
		t.Fatalf("active beat unexpectedly collapsed at test start")
	}

	tab(m, false)

	if !m.beats[active].Collapsed {
		t.Errorf("Tab did not collapse the active beat")
	}
	if !m.beats[active].UserOverride {
		t.Errorf("Tab did not set UserOverride on the active beat")
	}

	// Tab again — expands.
	tab(m, false)
	if m.beats[active].Collapsed {
		t.Errorf("second Tab did not expand the active beat")
	}
}

// TestTab_ExpandedBeatSurvivesNextTransition verifies the
// UserOverride contract: a user-expanded past beat stays expanded
// when a new beat opens, even though the auto-collapse policy would
// normally close it.
func TestTab_ExpandedBeatSurvivesNextTransition(t *testing.T) {
	m := beatPane()
	m.AppendUserMessage("first")
	m.AppendToken("reply\n")
	m.AppendUserMessage("second") // auto-collapses beat[1]
	m.AppendToken("agent prose\n")

	// Scroll up so the focus default lands on the past beat instead of
	// the active one. handleKey treats focus as "topmost visible beat"
	// when focusBeat is -1.
	m.ScrollOffset = 0
	priorActive := 1
	if !m.beats[priorActive].Collapsed {
		t.Fatalf("prior beat was not auto-collapsed; cannot verify the override case")
	}
	m.focusBeat = priorActive
	tab(m, false) // expand it
	if m.beats[priorActive].Collapsed {
		t.Fatalf("Tab did not expand the past beat")
	}

	// Now open a new beat. Auto-policy must not re-collapse the past
	// beat because UserOverride is set.
	m.AppendUserMessage("third")
	if m.beats[priorActive].Collapsed {
		t.Errorf("auto-collapse re-collapsed a user-expanded beat (UserOverride must block)")
	}
}

// TestShiftTab_CollapsesAndMovesFocus verifies Shift+Tab collapses the
// focused beat and moves focus to the previous beat. The key gesture
// is "collapse this, go back" — a quick way to fold an exhausted beat
// while scanning backwards.
func TestShiftTab_CollapsesAndMovesFocus(t *testing.T) {
	m := beatPane()
	m.AppendUserMessage("first")
	m.AppendToken("a\n")
	m.AppendUserMessage("second")
	m.AppendToken("b\n")

	// Focus the active beat explicitly.
	active := len(m.beats) - 1
	m.focusBeat = active

	tab(m, true)

	if !m.beats[active].Collapsed {
		t.Errorf("Shift+Tab did not collapse the focused beat")
	}
	if got, want := m.focusBeat, active-1; got != want {
		t.Errorf("focus = %d after Shift+Tab; want %d (previous beat)", got, want)
	}
}

// TestShiftTab_FirstBeat_NoOp verifies Shift+Tab on the first beat is
// safe: it collapses the first beat if it wasn't, but doesn't move
// focus to a negative index.
func TestShiftTab_FirstBeat_NoOp(t *testing.T) {
	m := beatPane()
	m.focusBeat = 0
	tab(m, true)
	if got := m.focusBeat; got != 0 {
		t.Errorf("focus moved off beat 0 to %d; want stay at 0", got)
	}
}
