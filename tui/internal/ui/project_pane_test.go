package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/latebit-io/junto/engine/project"
)

// mockProjectSession provides the minimal session surface for testing.
// We can't use the real session without disk I/O, so we test the tree
// and rendering logic through the public tree functions and verify the
// pane's key handling produces the right commands.

func TestProjectPane_RenderSections(t *testing.T) {
	// Build a pane-like structure manually using the tree functions
	// to verify rendering logic without a real session.

	items := []projectItem{
		{isHeader: true, section: "work"},
		{isHeader: true, section: "files"},
		{node: &TreeNode{Name: "main.go", Path: "main.go", Badge: "ctx"}, section: "files"},
		{node: &TreeNode{Name: "src", Path: "src", IsDir: true, Collapsed: true}, section: "files"},
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
	if !strings.Contains(output, "WORK") {
		t.Error("missing WORK header")
	}
	if !strings.Contains(output, "FILES") {
		t.Error("missing FILES header")
	}
}

func TestProjectPane_CursorMovement(t *testing.T) {
	items := []projectItem{
		{isHeader: true, section: "work"},
		{node: &TreeNode{Name: "a.go", Path: "a.go"}, section: "files"},
		{node: &TreeNode{Name: "b.go", Path: "b.go"}, section: "files"},
		{isHeader: true, section: "files"},
		{node: &TreeNode{Name: "c.go", Path: "c.go"}, section: "files"},
	}

	p := &ProjectPaneModel{
		width:     40,
		height:    10,
		items:     items,
		cursorIdx: 1, // start on a.go
	}

	// Move down
	p.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if p.cursorIdx != 2 {
		t.Errorf("expected cursor at 2 (b.go), got %d", p.cursorIdx)
	}

	// Move down — should skip header (index 3) and land on c.go (index 4)
	p.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if p.cursorIdx != 4 {
		t.Errorf("expected cursor at 4 (c.go), got %d", p.cursorIdx)
	}

	// Move down at end — should stay
	p.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if p.cursorIdx != 4 {
		t.Errorf("expected cursor to stay at 4, got %d", p.cursorIdx)
	}

	// Move up back to b.go
	p.handleKey(tea.KeyPressMsg{Code: tea.KeyUp})
	if p.cursorIdx != 2 {
		t.Errorf("expected cursor at 2 (b.go), got %d", p.cursorIdx)
	}
}

func TestProjectPane_CursorSkipsHeaders(t *testing.T) {
	items := []projectItem{
		{isHeader: true, section: "work"},
		{isHeader: true, section: "files"},
		{node: &TreeNode{Name: "a.go", Path: "a.go"}, section: "files"},
	}

	p := &ProjectPaneModel{
		width:     40,
		height:    10,
		items:     items,
		cursorIdx: 2, // on a.go
	}

	// Move up — all above are headers, should stay
	p.handleKey(tea.KeyPressMsg{Code: tea.KeyUp})
	if p.cursorIdx != 2 {
		t.Errorf("expected cursor to stay at 2, got %d", p.cursorIdx)
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
		{isHeader: true, section: "files"},
		{node: dir, section: "files"},
	}

	p := &ProjectPaneModel{
		width:         40,
		height:        10,
		items:         items,
		filesTree:     &TreeNode{IsDir: true, Children: []*TreeNode{dir}},
		workCollapsed: make(map[string]bool),
		cursorIdx:     1,
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

func TestProjectPane_ActivateWorkHeading(t *testing.T) {
	heading := &project.Node{
		Title: "Phase 6", Depth: 1, IsHeading: true,
		Children: []*project.Node{
			{Title: "task one", Depth: 2, Status: project.TaskPending},
		},
	}

	items := []projectItem{
		{isHeader: true, section: "work"},
		{workNode: heading, section: "work"},
		{workNode: heading.Children[0], section: "work"},
	}

	p := &ProjectPaneModel{
		width:         40,
		height:        10,
		items:         items,
		workCollapsed: make(map[string]bool),
		cursorIdx:     1, // on heading
	}

	// Activate heading — should toggle collapse
	cmd := p.activateItem()
	if cmd != nil {
		t.Error("expected nil cmd for heading toggle")
	}
	if !p.workCollapsed["Phase 6"] {
		t.Error("expected heading to be collapsed after activation")
	}

	// Activate again — should uncollapse
	// Reset items since flattenItems was called
	p.cursorIdx = 1
	p.items = items
	p.activateItem()
	if p.workCollapsed["Phase 6"] {
		t.Error("expected heading to be uncollapsed after second activation")
	}
}

func TestProjectPane_ActivateWorkTask(t *testing.T) {
	task := &project.Node{
		Title: "Add Phase to Session", Depth: 2, Status: project.TaskPending,
	}

	items := []projectItem{
		{isHeader: true, section: "work"},
		{workNode: task, section: "work"},
	}

	p := &ProjectPaneModel{
		width:         40,
		height:        10,
		items:         items,
		workCollapsed: make(map[string]bool),
		cursorIdx:     1, // on task
	}

	cmd := p.activateItem()
	if cmd == nil {
		t.Fatal("expected cmd for task activation")
	}

	msg := cmd()
	goalMsg, ok := msg.(ProjectSetActiveGoalMsg)
	if !ok {
		t.Fatalf("expected ProjectSetActiveGoalMsg, got %T", msg)
	}
	if goalMsg.Title != "Add Phase to Session" {
		t.Errorf("Title = %q, want %q", goalMsg.Title, "Add Phase to Session")
	}
}

func TestProjectPane_ActivateActiveTask_MarksDone(t *testing.T) {
	task := &project.Node{
		Title: "Complete this", Depth: 2, Status: project.TaskActive,
	}

	items := []projectItem{
		{isHeader: true, section: "work"},
		{workNode: task, section: "work"},
	}

	p := &ProjectPaneModel{
		width:         40,
		height:        10,
		items:         items,
		workCollapsed: make(map[string]bool),
		cursorIdx:     1,
	}

	cmd := p.activateItem()
	if cmd == nil {
		t.Fatal("expected cmd for active task")
	}

	msg := cmd()
	doneMsg, ok := msg.(ProjectMarkGoalDoneMsg)
	if !ok {
		t.Fatalf("expected ProjectMarkGoalDoneMsg, got %T", msg)
	}
	if doneMsg.Title != "Complete this" {
		t.Errorf("Title = %q, want %q", doneMsg.Title, "Complete this")
	}
}

func TestProjectPane_ActivateHeaderNoop(t *testing.T) {
	items := []projectItem{
		{isHeader: true, section: "files"},
		{node: &TreeNode{Name: "a.go", Path: "a.go"}, section: "files"},
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
		{isHeader: true, section: "files"},
		{node: &TreeNode{Name: "a.go", Path: "a.go"}, section: "files"},
		{node: &TreeNode{Name: "b.go", Path: "b.go"}, section: "files"},
	}

	p := &ProjectPaneModel{
		width:     40,
		height:    10,
		items:     items,
		cursorIdx: 1,
	}

	// j moves down
	p.handleKey(tea.KeyPressMsg{Code: 'j', Text: "j"})
	if p.cursorIdx != 2 {
		t.Errorf("j: expected cursor at 2, got %d", p.cursorIdx)
	}

	// k moves up
	p.handleKey(tea.KeyPressMsg{Code: 'k', Text: "k"})
	if p.cursorIdx != 1 {
		t.Errorf("k: expected cursor at 1, got %d", p.cursorIdx)
	}
}

func TestProjectPane_ScrollClamp(t *testing.T) {
	var items []projectItem
	for i := range 20 {
		items = append(items, projectItem{
			node:    &TreeNode{Name: strings.Repeat("x", i+1), Path: strings.Repeat("x", i+1)},
			section: "files",
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
