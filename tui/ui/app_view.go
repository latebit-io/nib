package ui

import (
	tea "charm.land/bubbletea/v2"
)

// Top-level View rendering.
// Extracted from app.go (Phase 8 of AppModel decomposition).
//
// Layout: intent bar (1 row) + region-managed pane area + status bar.
// Modal overlays (Dialog, Help, Palette, SearchOverlay) render on top
// of the base layout via their own RenderOverlay implementations. The
// modal precedence here mirrors the input-gating order in
// `app_modal.go.handleModalInput`.

// View renders the top-level layout: intent bar + region area + status
// bar, with any active modal overlay composited on top.
func (m *AppModel) View() tea.View {
	var content string
	if m.Width == 0 || m.Height == 0 {
		content = "Initializing..."
	} else {
		mem := m.Session.DistributedMemory()
		extra := 3 // dial + terse + usage always shown
		indicators := make([]string, len(mem)+extra)
		copy(indicators, mem)
		idx := len(mem)
		indicators[idx] = m.dial.String()
		idx++
		if m.terse {
			indicators[idx] = "terse:on"
		} else {
			indicators[idx] = "terse:off"
		}
		idx++
		indicators[idx] = m.AgentPane.UsageIndicator()
		base := m.renderIntentBar() + "\n" + m.Regions.Render() + "\n" + renderStatusBar(m.Editor.statusInfo(), m.Width, indicators...)
		if m.Dialog.Active {
			content = m.Dialog.RenderOverlay(base, m.Width, m.Height)
		} else if m.Help.Active {
			content = m.Help.RenderOverlay(base, m.Width, m.Height)
		} else if m.Palette.Active {
			content = m.Palette.RenderOverlay(base, m.Width, m.Height)
		} else if m.SearchOverlay.Active {
			content = m.SearchOverlay.RenderOverlay(base, m.Width, m.Height)
		} else {
			content = base
		}
	}

	v := tea.NewView(content)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}
