package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/latebit-io/nib/engine/project"
	"github.com/latebit-io/nib/tui/internal/ui/textarea"
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
		width:        40,
		height:       10,
		items:        items,
		filesTree:    &TreeNode{IsDir: true, Children: []*TreeNode{dir}},
		workExpanded: make(map[string]bool),
		cursorIdx:    1,
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
		width:        40,
		height:       10,
		items:        items,
		workExpanded: make(map[string]bool),
		cursorIdx:    1, // on heading
	}

	// Activate heading — should toggle to expanded
	cmd := p.activateItem()
	if cmd != nil {
		t.Error("expected nil cmd for heading toggle")
	}
	if !p.workExpanded["Phase 6"] {
		t.Error("expected heading to be expanded after activation")
	}

	// Activate again — should collapse
	// Reset items since flattenItems was called
	p.cursorIdx = 1
	p.items = items
	p.activateItem()
	if p.workExpanded["Phase 6"] {
		t.Error("expected heading to be collapsed after second activation")
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
		width:        40,
		height:       10,
		items:        items,
		workExpanded: make(map[string]bool),
		cursorIdx:    1, // on task
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
		width:        40,
		height:       10,
		items:        items,
		workExpanded: make(map[string]bool),
		cursorIdx:    1,
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

func TestProjectPane_StartFileCreate_OnDir(t *testing.T) {
	dir := &TreeNode{Name: "src", Path: "src", IsDir: true, Collapsed: true}

	p := &ProjectPaneModel{
		width:        40,
		height:       10,
		items:        []projectItem{{isHeader: true, section: "files"}, {node: dir, section: "files"}},
		filesTree:    &TreeNode{IsDir: true, Children: []*TreeNode{dir}},
		workExpanded: make(map[string]bool),
		createInput:  textarea.New(1),
		cursorIdx:    1,
	}

	p.startFileCreate()

	if !p.creatingFile {
		t.Error("expected creatingFile to be true")
	}
	if p.createDirPath != "src" {
		t.Errorf("createDirPath = %q, want %q", p.createDirPath, "src")
	}
	// Directory should be expanded
	if dir.Collapsed {
		t.Error("directory should be expanded when creating file inside it")
	}
}

func TestProjectPane_StartFileCreate_OnFile(t *testing.T) {
	file := &TreeNode{Name: "main.go", Path: "src/main.go"}

	p := &ProjectPaneModel{
		width:        40,
		height:       10,
		items:        []projectItem{{isHeader: true, section: "files"}, {node: file, section: "files"}},
		workExpanded: make(map[string]bool),
		createInput:  textarea.New(1),
		cursorIdx:    1,
	}

	p.startFileCreate()

	if !p.creatingFile {
		t.Error("expected creatingFile to be true")
	}
	if p.createDirPath != "src" {
		t.Errorf("createDirPath = %q, want %q", p.createDirPath, "src")
	}
}

func TestProjectPane_StartFileCreate_RootLevel(t *testing.T) {
	file := &TreeNode{Name: "main.go", Path: "main.go"}

	p := &ProjectPaneModel{
		width:        40,
		height:       10,
		items:        []projectItem{{isHeader: true, section: "files"}, {node: file, section: "files"}},
		workExpanded: make(map[string]bool),
		createInput:  textarea.New(1),
		cursorIdx:    1,
	}

	p.startFileCreate()

	if p.createDirPath != "" {
		t.Errorf("createDirPath = %q, want empty for root level", p.createDirPath)
	}
}

func TestProjectPane_HandleCreateInput_Submit(t *testing.T) {
	p := &ProjectPaneModel{
		width:         40,
		height:        10,
		creatingFile:  true,
		createDirPath: "src",
		createInput:   textarea.New(1),
		workExpanded:  make(map[string]bool),
	}
	p.createInput.SetSize(40)
	p.createInput.SetContent("new_file.go")

	cmd := p.handleCreateInput(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected cmd on submit")
	}

	msg := cmd()
	createMsg, ok := msg.(ProjectCreateFileMsg)
	if !ok {
		t.Fatalf("expected ProjectCreateFileMsg, got %T", msg)
	}
	if createMsg.Path != "src/new_file.go" {
		t.Errorf("Path = %q, want %q", createMsg.Path, "src/new_file.go")
	}
	if p.creatingFile {
		t.Error("creatingFile should be false after submit")
	}
}

func TestProjectPane_HandleCreateInput_Cancel(t *testing.T) {
	p := &ProjectPaneModel{
		width:        40,
		height:       10,
		creatingFile: true,
		createInput:  textarea.New(1),
		workExpanded: make(map[string]bool),
	}

	p.handleCreateInput(tea.KeyPressMsg{Code: tea.KeyEscape})

	if p.creatingFile {
		t.Error("creatingFile should be false after cancel")
	}
}

func TestProjectPane_StartDirCreate(t *testing.T) {
	dir := &TreeNode{Name: "src", Path: "src", IsDir: true, Collapsed: true}

	p := &ProjectPaneModel{
		width:        40,
		height:       10,
		items:        []projectItem{{isHeader: true, section: "files"}, {node: dir, section: "files"}},
		filesTree:    &TreeNode{IsDir: true, Children: []*TreeNode{dir}},
		workExpanded: make(map[string]bool),
		createInput:  textarea.New(1),
		cursorIdx:    1,
	}

	p.startDirCreate()

	if !p.creatingFile {
		t.Error("expected creatingFile to be true")
	}
	if !p.creatingDir {
		t.Error("expected creatingDir to be true")
	}
	if p.createDirPath != "src" {
		t.Errorf("createDirPath = %q, want %q", p.createDirPath, "src")
	}
}

func TestProjectPane_HandleCreateInput_SubmitDir(t *testing.T) {
	p := &ProjectPaneModel{
		width:         40,
		height:        10,
		creatingFile:  true,
		creatingDir:   true,
		createDirPath: "src",
		createInput:   textarea.New(1),
		workExpanded:  make(map[string]bool),
	}
	p.createInput.SetSize(40)
	p.createInput.SetContent("pkg")

	cmd := p.handleCreateInput(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected cmd on submit")
	}

	msg := cmd()
	dirMsg, ok := msg.(ProjectCreateDirMsg)
	if !ok {
		t.Fatalf("expected ProjectCreateDirMsg, got %T", msg)
	}
	if dirMsg.Path != "src/pkg" {
		t.Errorf("Path = %q, want %q", dirMsg.Path, "src/pkg")
	}
	if p.creatingDir {
		t.Error("creatingDir should be false after submit")
	}
}

func TestProjectPane_StartFileDelete(t *testing.T) {
	file := &TreeNode{Name: "victim.go", Path: "src/victim.go"}

	p := &ProjectPaneModel{
		width:     40,
		height:    10,
		items:     []projectItem{{isHeader: true, section: "files"}, {node: file, section: "files"}},
		cursorIdx: 1,
	}

	p.startFileDelete()

	if !p.confirmDelete {
		t.Error("expected confirmDelete to be true")
	}
	if p.confirmDeletePath != "src/victim.go" {
		t.Errorf("confirmDeletePath = %q, want %q", p.confirmDeletePath, "src/victim.go")
	}
}

func TestProjectPane_ConfirmDelete_Yes(t *testing.T) {
	p := &ProjectPaneModel{
		width:             40,
		height:            10,
		confirmDelete:     true,
		confirmDeletePath: "src/victim.go",
	}

	cmd := p.handleConfirmDelete(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if cmd == nil {
		t.Fatal("expected cmd on confirm")
	}
	msg := cmd()
	deleteMsg, ok := msg.(ProjectDeleteFileMsg)
	if !ok {
		t.Fatalf("expected ProjectDeleteFileMsg, got %T", msg)
	}
	if deleteMsg.Path != "src/victim.go" {
		t.Errorf("Path = %q, want %q", deleteMsg.Path, "src/victim.go")
	}
	if p.confirmDelete {
		t.Error("confirmDelete should be false after confirm")
	}
}

func TestProjectPane_ConfirmDelete_No(t *testing.T) {
	p := &ProjectPaneModel{
		width:             40,
		height:            10,
		confirmDelete:     true,
		confirmDeletePath: "src/victim.go",
	}

	cmd := p.handleConfirmDelete(tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd != nil {
		t.Error("expected nil cmd on cancel")
	}
	if p.confirmDelete {
		t.Error("confirmDelete should be false after cancel")
	}
}

func TestProjectPane_StartFileDelete_OnDir_Noop(t *testing.T) {
	dir := &TreeNode{Name: "src", Path: "src", IsDir: true}

	p := &ProjectPaneModel{
		width:     40,
		height:    10,
		items:     []projectItem{{isHeader: true, section: "files"}, {node: dir, section: "files"}},
		cursorIdx: 1,
	}

	cmd := p.startFileDelete()
	if cmd != nil {
		t.Error("expected nil cmd when deleting a directory")
	}
}

func TestProjectPane_IsInputActive(t *testing.T) {
	p := &ProjectPaneModel{}
	if p.IsInputActive() {
		t.Error("should not be active by default")
	}
	p.creatingFile = true
	if !p.IsInputActive() {
		t.Error("should be active when creatingFile is true")
	}
}
