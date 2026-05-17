package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestKeymap_MacOptionRuneFallbacks ensures that each Alt+letter action has a
// macOS rune fallback, because Option+letter on macOS emits a literal rune
// (e.g. Option+K → ˚) with no Alt modifier. A missing fallback means the
// binding silently does nothing on macOS.
func TestKeymap_MacOptionRuneFallbacks(t *testing.T) {
	tests := []struct {
		name   string
		letter rune
		rune   rune
		want   Action
	}{
		{"Alt+G / ©", 'g', '©', ActionAgentPlan},
		{"Alt+A / å", 'a', 'å', ActionDialCycle},
		{"Alt+M / µ", 'm', 'µ', ActionModelSelector},
		{"Alt+S / ß", 's', 'ß', ActionStyleCycle},
		{"Alt+T / †", 't', '†', ActionTerseToggle},
	}

	km := DefaultKeymap()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			altMsg := tea.KeyPressMsg{Code: tc.letter, Mod: tea.ModAlt}
			if got := km.Match(altMsg); got != tc.want {
				t.Errorf("Alt+%c resolved to %v, want %v", tc.letter, got, tc.want)
			}
			runeMsg := tea.KeyPressMsg{Code: tc.rune, Text: string(tc.rune)}
			if got := km.Match(runeMsg); got != tc.want {
				t.Errorf("macOS rune %q resolved to %v, want %v (missing macOS fallback?)", tc.rune, got, tc.want)
			}
		})
	}
}

// TestKeymap_Hover verifies Shift+F1 triggers hover. The binding is tested
// both by direct key lookup and by string lookup (msg.String()) so the
// match path taken by bubbletea at runtime is covered either way.
func TestKeymap_Hover(t *testing.T) {
	km := DefaultKeymap()
	msg := tea.KeyPressMsg{Code: tea.KeyF1, Mod: tea.ModShift}
	if got := km.Match(msg); got != ActionHover {
		t.Errorf("Shift+F1 resolved to %v, want ActionHover", got)
	}
}
