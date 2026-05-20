package ui

import (
	"image/color"
	"strings"
	"testing"

	"github.com/latebit-io/nib/coding/event"
	"github.com/latebit-io/nib/tui/ui/theme"
)

// TestInputStateHintLine_PrecedenceOrder verifies the four-state hint
// line picks running → focused → empty-placeholder → legacy slash-hint
// in that order. The precedence matters: an animating agent must win
// over focus state, because the user looking at the pane needs to see
// "agent is busy / esc interrupt" rather than the generic send hint.
func TestInputStateHintLine_PrecedenceOrder(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(*AgentPaneModel)
		wantSubs string // substring the hint must contain
	}{
		{
			"running wins over focused",
			func(m *AgentPaneModel) {
				m.SetStatus(event.StatusThinking)
				m.SetInputActive(true)
			},
			"nib is working",
		},
		{
			"focused with content shows send hint",
			func(m *AgentPaneModel) {
				m.SetInputActive(true)
				m.input.SetContent("typing something")
			},
			"enter send",
		},
		{
			"idle empty shows placeholder",
			func(m *AgentPaneModel) {},
			"ask nib something",
		},
		{
			"idle with content falls through to slash hint",
			func(m *AgentPaneModel) {
				m.input.SetContent("leftover")
			},
			"Ctrl+G",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := beatPane()
			tc.setup(m)
			hint := m.inputStateHintLine()
			if !strings.Contains(hint, tc.wantSubs) {
				t.Errorf("hint = %q; want substring %q", hint, tc.wantSubs)
			}
		})
	}
}

// TestInputStateDividerColor_StateAware verifies the hairline divider
// above the input takes its color from input state. Catches a
// regression where the divider falls back to dim gray during focus or
// streaming — losing the cheapest state indicator in the pane.
func TestInputStateDividerColor_StateAware(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*AgentPaneModel)
		want  color.Color
	}{
		{
			"idle → DividerActive",
			func(m *AgentPaneModel) {},
			theme.DividerActive,
		},
		{
			"focused → Accent",
			func(m *AgentPaneModel) { m.SetInputActive(true) },
			theme.Accent,
		},
		{
			"running → Warning",
			func(m *AgentPaneModel) { m.SetStatus(event.StatusThinking) },
			theme.Warning,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := beatPane()
			tc.setup(m)
			got := m.inputStateDividerStyle().GetForeground()
			if got != tc.want {
				t.Errorf("divider style foreground = %v; want %v", got, tc.want)
			}
		})
	}
}
