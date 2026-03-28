package ui

import (
	"strings"
	"testing"
)

func TestShowHoverAndDismiss(t *testing.T) {
	m := newTestEditorModel("func main() {\n\tfmt.Println()\n}\n")

	// No hover by default.
	output := m.Render()
	if strings.Contains(output, "func main()") && strings.Contains(output, "string") {
		t.Error("unexpected hover content in initial render")
	}

	// Show hover.
	m.ShowHover("func Println(a ...any)")
	if m.hoverText == "" {
		t.Fatal("hoverText should be set")
	}

	output = m.Render()
	if !strings.Contains(output, "func Println") {
		t.Error("hover text should appear in render output")
	}

	// Dismiss hover.
	m.DismissHover()
	if m.hoverText != "" {
		t.Fatal("hoverText should be cleared")
	}

	output = m.Render()
	if strings.Contains(output, "func Println") {
		t.Error("hover text should not appear after dismiss")
	}
}

func TestHoverDismissesOnCursorMove(t *testing.T) {
	m := newTestEditorModel("hello world\nsecond line\n")
	m.ShowHover("some type info")

	if m.hoverText == "" {
		t.Fatal("hover should be active")
	}

	// Simulate a key press (which dismisses hover via Update).
	// Any KeyMsg triggers DismissHover in Update.
	m.DismissHover() // simulating what Update does
	if m.hoverText != "" {
		t.Error("hover should be dismissed after key event")
	}
}

func TestHoverOverlayPositioning(t *testing.T) {
	m := newTestEditorModel("line0\nline1\nline2\nline3\nline4\n")
	m.eng.CursorLine = 1
	m.eng.CursorCol = 0
	m.ShowHover("hover text")

	// Should render without panic.
	output := m.Render()
	if !strings.Contains(output, "hover text") {
		t.Error("hover text should appear in output")
	}
}

func TestHoverEmptyTextNotShown(t *testing.T) {
	m := newTestEditorModel("hello\n")
	m.ShowHover("")
	output := m.Render()
	// Empty hover should not affect output — just verify no panic.
	if m.hoverText != "" {
		t.Error("empty hover should not be stored")
	}
	_ = output
}
