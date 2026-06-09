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

// TestTab_TogglesFocusedBeat verifies plain Tab on an explicitly
// focused beat flips its collapse flag and stamps UserOverride so
// the auto policy can't undo it on the next transition.
func TestTab_TogglesFocusedBeat(t *testing.T) {
	m := beatPane()
	m.AppendUserMessage("hello")
	m.AppendToken("agent reply\n")

	// Pin focus to the active beat so the test is independent of the
	// default-focus policy (which prefers the previous beat at bottom).
	target := len(m.beats) - 1
	m.focusBeat = target
	if m.beats[target].Collapsed {
		t.Fatalf("target beat unexpectedly collapsed at test start")
	}

	tab(m, false)

	if !m.beats[target].Collapsed {
		t.Errorf("Tab did not collapse the focused beat")
	}
	if !m.beats[target].UserOverride {
		t.Errorf("Tab did not set UserOverride on the focused beat")
	}

	// Tab again — expands.
	tab(m, false)
	if m.beats[target].Collapsed {
		t.Errorf("second Tab did not expand the focused beat")
	}
}

// TestTab_AtBottom_DefaultsToPreviousBeat asserts the bottom-of-pane
// default-focus rule: with no explicit focus, Tab at the bottom of
// the viewport targets the most recent *past* beat (the one that
// just got collapsed), not the active beat. This is the natural
// "expand the previous exchange" gesture for someone typing the
// next message.
func TestTab_AtBottom_DefaultsToPreviousBeat(t *testing.T) {
	m := beatPane()
	m.AppendUserMessage("first")
	m.AppendToken("first reply\n")
	m.AppendUserMessage("second")
	m.AppendToken("second reply\n")

	// First user→agent beat (beats[1]) auto-collapsed when the second
	// message arrived. Active beat is beats[2]. With no explicit focus
	// and isAtBottom() true, Tab should expand beats[1].
	previous := len(m.beats) - 2
	if !m.beats[previous].Collapsed {
		t.Fatalf("previous beat is not collapsed; can't verify default-focus expand")
	}
	m.focusBeat = -1

	tab(m, false)

	if m.beats[previous].Collapsed {
		t.Errorf("Tab at bottom did not expand the previous beat (got Collapsed=true)")
	}
	active := len(m.beats) - 1
	if m.beats[active].Collapsed {
		t.Errorf("Tab at bottom incorrectly toggled the active beat")
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

// TestClick_ExpandsCollapsedSummaryRow verifies the mouse gesture for
// expanding past beats: a left click that lands on a beat-summary row
// expands the underlying beat in place. This is the discoverable
// alternative to the Tab keybind — most developers don't think to Esc
// out of the textarea first to use Tab, so the click has to work
// directly.
func TestClick_ExpandsCollapsedSummaryRow(t *testing.T) {
	m := beatPane()
	m.AppendUserMessage("first")
	m.AppendToken("hidden\n")
	m.AppendUserMessage("second")
	m.AppendToken("active\n")

	proj := m.projection()
	summaryRow := -1
	for i, pl := range proj {
		if pl.Kind == projBeatSummary {
			summaryRow = i
			break
		}
	}
	if summaryRow < 0 {
		t.Fatalf("no summary row to click")
	}
	beatIdx := proj[summaryRow].BeatIdx
	if !m.beats[beatIdx].Collapsed {
		t.Fatalf("target beat not collapsed; cannot test expand-on-click")
	}

	// Synthesize a left-click at the summary row. Y is the visible
	// row inside the content area, which matches the projection-row
	// index since ScrollOffset is 0 (transcript fits in viewport).
	m.handleMouseClick(tea.MouseClickMsg{
		Button: tea.MouseLeft,
		X:      2,
		Y:      summaryRow - m.ScrollOffset,
	})

	if m.beats[beatIdx].Collapsed {
		t.Errorf("click on summary row did not expand the beat")
	}
	if !m.beats[beatIdx].UserOverride {
		t.Errorf("click on summary row did not set UserOverride")
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
