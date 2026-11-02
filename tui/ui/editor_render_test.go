package ui

import (
	"testing"

	"charm.land/lipgloss/v2"
)

func TestSyntaxForeground(t *testing.T) {
	if fg := syntaxForeground(lipgloss.NewStyle()); fg != nil {
		t.Errorf("unset foreground = %v, want nil (lipgloss reports NoColor{}, which must not count as syntax color)", fg)
	}
	red := lipgloss.Color("#ff0000")
	if fg := syntaxForeground(lipgloss.NewStyle().Foreground(red)); fg != red {
		t.Errorf("set foreground = %v, want %v", fg, red)
	}
}
