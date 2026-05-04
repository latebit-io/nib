package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func testItems() []PaletteItem {
	return []PaletteItem{
		{Label: "engine/session/session.go", Category: "file", Value: "/abs/engine/session/session.go"},
		{Label: "engine/editor/editor.go", Category: "file", Value: "/abs/engine/editor/editor.go"},
		{Label: "tui/internal/ui/app.go", Category: "file", Value: "/abs/tui/internal/ui/app.go"},
		{Label: "main.go", Category: "file", Value: "/abs/main.go"},
	}
}

// keyPress creates a KeyPressMsg for a printable character.
func keyPress(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

// specialKey creates a KeyPressMsg for a special key (no text).
func specialKey(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code}
}

func TestPalette_Open(t *testing.T) {
	var p PaletteModel
	p.Open(testItems())

	if !p.Active {
		t.Error("palette should be active after Open")
	}
	if p.Query != "" {
		t.Error("query should be empty after Open")
	}
	if len(p.Filtered) != 4 {
		t.Errorf("filtered = %d, want 4 (all items)", len(p.Filtered))
	}
	if p.Selected != 0 {
		t.Error("selected should be 0 after Open")
	}
}

func TestPalette_FilterOnType(t *testing.T) {
	var p PaletteModel
	p.Open(testItems())

	// Type "ses" — should match session.go.
	for _, r := range "ses" {
		p.Update(keyPress(r))
	}

	if p.Query != "ses" {
		t.Errorf("query = %q, want %q", p.Query, "ses")
	}
	if len(p.Filtered) == 0 {
		t.Fatal("expected matches for 'ses'")
	}
	if p.Filtered[0].Text != "engine/session/session.go" {
		t.Errorf("first match = %q, want session.go path", p.Filtered[0].Text)
	}
}

func TestPalette_Backspace(t *testing.T) {
	var p PaletteModel
	p.Open(testItems())

	for _, r := range "ses" {
		p.Update(keyPress(r))
	}
	p.Update(specialKey(tea.KeyBackspace))

	if p.Query != "se" {
		t.Errorf("query after backspace = %q, want %q", p.Query, "se")
	}
}

func TestPalette_BackspaceEmpty(t *testing.T) {
	var p PaletteModel
	p.Open(testItems())

	// Backspace on empty query should not panic.
	p.Update(specialKey(tea.KeyBackspace))
	if p.Query != "" {
		t.Errorf("query = %q, want empty", p.Query)
	}
}

func TestPalette_Navigation(t *testing.T) {
	var p PaletteModel
	p.Open(testItems())

	// Down from 0.
	p.Update(specialKey(tea.KeyDown))
	if p.Selected != 1 {
		t.Errorf("selected after down = %d, want 1", p.Selected)
	}

	// Up back to 0.
	p.Update(specialKey(tea.KeyUp))
	if p.Selected != 0 {
		t.Errorf("selected after up = %d, want 0", p.Selected)
	}

	// Up at 0 should stay at 0 (no wrap).
	p.Update(specialKey(tea.KeyUp))
	if p.Selected != 0 {
		t.Errorf("selected at top after up = %d, want 0", p.Selected)
	}
}

func TestPalette_Enter(t *testing.T) {
	var p PaletteModel
	p.Open(testItems())

	cmd := p.Update(specialKey(tea.KeyEnter))
	if cmd == nil {
		t.Fatal("expected cmd from Enter")
	}

	msg := cmd()
	result, ok := msg.(PaletteResultMsg)
	if !ok {
		t.Fatalf("expected PaletteResultMsg, got %T", msg)
	}
	if result.Cancelled {
		t.Error("result should not be cancelled")
	}
	if result.Category != "file" {
		t.Errorf("category = %q, want %q", result.Category, "file")
	}
	// Close is called internally — palette should be inactive after Enter.
	if p.Active {
		t.Error("palette should be inactive after Enter")
	}
}

func TestPalette_Escape(t *testing.T) {
	var p PaletteModel
	p.Open(testItems())

	cmd := p.Update(specialKey(tea.KeyEscape))
	if cmd == nil {
		t.Fatal("expected cmd from Escape")
	}

	msg := cmd()
	result, ok := msg.(PaletteResultMsg)
	if !ok {
		t.Fatalf("expected PaletteResultMsg, got %T", msg)
	}
	if !result.Cancelled {
		t.Error("escape should set Cancelled = true")
	}
	if p.Active {
		t.Error("palette should be inactive after Escape")
	}
}

func TestPalette_Close(t *testing.T) {
	var p PaletteModel
	p.Open(testItems())
	p.Close()

	if p.Active {
		t.Error("palette should be inactive after Close")
	}
	if p.Items != nil {
		t.Error("items should be nil after Close")
	}
}

func TestPalette_EmptyResults(t *testing.T) {
	var p PaletteModel
	p.Open(testItems())

	// Type something that matches nothing.
	for _, r := range "zzzzz" {
		p.Update(keyPress(r))
	}

	if len(p.Filtered) != 0 {
		t.Errorf("expected 0 results, got %d", len(p.Filtered))
	}

	// Enter with no results should be a no-op.
	cmd := p.Update(specialKey(tea.KeyEnter))
	if cmd != nil {
		t.Error("Enter with no results should return nil cmd")
	}
}

func TestPalette_FilteredSelection(t *testing.T) {
	var p PaletteModel
	p.Open(testItems())

	// Type "app" to filter, then select with Enter.
	for _, r := range "app" {
		p.Update(keyPress(r))
	}

	if len(p.Filtered) == 0 {
		t.Fatal("expected matches for 'app'")
	}

	cmd := p.Update(specialKey(tea.KeyEnter))
	if cmd == nil {
		t.Fatal("expected cmd from Enter")
	}

	msg := cmd()
	result := msg.(PaletteResultMsg)
	if result.Item.Value != "/abs/tui/internal/ui/app.go" {
		t.Errorf("selected value = %q, want app.go path", result.Item.Value)
	}
}
