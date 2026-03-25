package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// mockProjectSession provides the minimal session surface for testing.
// We can't use the real session without disk I/O, so we test the tree
// and rendering logic through the public tree functions and verify the
// pane's key handling produces the right commands.

func TestProjectPane_RenderSections(t *testing.T) {
	// Build a pane-like structure manually using the tree functions
	// to verify rendering logic without a real session.

	items := []projectItem{
		{isHeader: true, section: "context"},
		{node: &TreeNode{Name: "main.go", Path: "main.go", Badge: "ctx"}, section: "context"},
		{isHeader: true, section: "review"},
		{isHeader: true, section: "modified"},
		{isHeader: true, section: "project"},
		{node: &TreeNode{Name: "src", Path: "src", IsDir: true, Collapsed: true}, section: "project"},
	}

	p := &ProjectPaneModel{
		width:  40,
		height: 10,
		items:  items,
	}

	output := p.Render()
	lines := strings.Split(output, "\n")

	if len(lines) != 10 {
		t.Errorf("expected 10 lines, got %d", len(lines))
	}

	// Check section headers are present
	if !strings.Contains(output, "CONTEXT") {
		t.Error("missing CONTEXT header")
	}
	if !strings.Contains(output, "REVIEW") {
		t.Error("missing REVIEW header")
	}
	if !strings.Contains(output, "MODIFIED") {
		t.Error("missing MODIFIED header")
	}
	if !strings.Contains(output, "PROJECT") {
		t.Error("missing PROJECT header")
	}
}

func TestProjectPane_CursorMovement(t *testing.T) {
	items := []projectItem{
		{isHeader: true, section: "context"},
		{node: &TreeNode{Name: "a.go", Path: "a.go"}, section: "context"},
		{node: &TreeNode{Name: "b.go", Path: "b.go"}, section: "context"},
		{isHeader: true, section: "review"},
		{node: &TreeNode{Name: "c.go", Path: "c.go"}, section: "project"},
	}

	p := &ProjectPaneModel{
		width:     40,
		height:    10,
		items:     items,
		cursorIdx: 1, // start on a.go
	}

	// Move down
	p.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if p.cursorIdx != 2 {
		t.Errorf("expected cursor at 2 (b.go), got %d", p.cursorIdx)
	}

	// Move down — should skip header (index 3) and land on c.go (index 4)
	p.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if p.cursorIdx != 4 {
		t.Errorf("expected cursor at 4 (c.go), got %d", p.cursorIdx)
	}

	// Move down at end — should stay
	p.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if p.cursorIdx != 4 {
		t.Errorf("expected cursor to stay at 4, got %d", p.cursorIdx)
	}

	// Move up back to b.go
	p.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if p.cursorIdx != 2 {
		t.Errorf("expected cursor at 2 (b.go), got %d", p.cursorIdx)
	}
}

func TestProjectPane_CursorSkipsHeaders(t *testing.T) {
	items := []projectItem{
		{isHeader: true, section: "context"},
		{isHeader: true, section: "review"},
		{isHeader: true, section: "modified"},
		{node: &TreeNode{Name: "a.go", Path: "a.go"}, section: "project"},
	}

	p := &ProjectPaneModel{
		width:     40,
		height:    10,
		items:     items,
		cursorIdx: 3, // on a.go
	}

	// Move up — all above are headers, should stay
	p.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if p.cursorIdx != 3 {
		t.Errorf("expected cursor to stay at 3, got %d", p.cursorIdx)
	}
}

func TestProjectPane_ToggleDirectory(t *testing.T) {
	dir := &TreeNode{Name: "src", Path: "src", IsDir: true, Collapsed: true,
		Children: []*TreeNode{
			{Name: "main.go", Path: "src/main.go"},
		},
	}
	// Set parent pointers
	for _, c := range dir.Children {
		c.parent = dir
	}

	items := []projectItem{
		{isHeader: true, section: "project"},
		{node: dir, section: "project"},
	}

	p := &ProjectPaneModel{
		width:       40,
		height:      10,
		items:       items,
		projectTree: &TreeNode{IsDir: true, Children: []*TreeNode{dir}},
		cursorIdx:   1,
	}

	// Activate (Enter) on collapsed dir — should expand
	p.activateItem()
	if dir.Collapsed {
		t.Error("expected dir to be expanded after Enter")
	}

	// Items should now include the child
	found := false
	for _, item := range p.items {
		if item.node != nil && item.node.Name == "main.go" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected main.go to appear after expanding src")
	}
}

func TestProjectPane_ActivateHeaderNoop(t *testing.T) {
	items := []projectItem{
		{isHeader: true, section: "context"},
		{node: &TreeNode{Name: "a.go", Path: "a.go"}, section: "context"},
	}

	p := &ProjectPaneModel{
		width:     40,
		height:    10,
		items:     items,
		cursorIdx: 0, // on header
	}

	cmd := p.activateItem()
	if cmd != nil {
		t.Error("expected nil cmd when activating a header")
	}
}

func TestProjectPane_VimKeys(t *testing.T) {
	items := []projectItem{
		{isHeader: true, section: "context"},
		{node: &TreeNode{Name: "a.go", Path: "a.go"}, section: "context"},
		{node: &TreeNode{Name: "b.go", Path: "b.go"}, section: "context"},
	}

	p := &ProjectPaneModel{
		width:     40,
		height:    10,
		items:     items,
		cursorIdx: 1,
	}

	// j moves down
	p.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if p.cursorIdx != 2 {
		t.Errorf("j: expected cursor at 2, got %d", p.cursorIdx)
	}

	// k moves up
	p.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if p.cursorIdx != 1 {
		t.Errorf("k: expected cursor at 1, got %d", p.cursorIdx)
	}
}

func TestProjectPane_ScrollClamp(t *testing.T) {
	var items []projectItem
	for i := range 20 {
		items = append(items, projectItem{
			node:    &TreeNode{Name: strings.Repeat("x", i+1), Path: strings.Repeat("x", i+1)},
			section: "project",
		})
	}

	p := &ProjectPaneModel{
		width:        40,
		height:       5,
		items:        items,
		scrollOffset: 100, // way past end
	}

	p.clampScroll()
	if p.scrollOffset != 15 { // 20 items - 5 height
		t.Errorf("expected scroll clamped to 15, got %d", p.scrollOffset)
	}

	p.scrollOffset = -5
	p.clampScroll()
	if p.scrollOffset != 0 {
		t.Errorf("expected scroll clamped to 0, got %d", p.scrollOffset)
	}
}

func TestProjectPane_EmptyRender(t *testing.T) {
	p := &ProjectPaneModel{
		width:  0,
		height: 0,
	}
	if p.Render() != "" {
		t.Error("expected empty render with zero dimensions")
	}
}
