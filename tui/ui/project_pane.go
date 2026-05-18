package ui

import (
	"errors"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/latebit-io/nib/engine/filelist"
	"github.com/latebit-io/nib/engine/project"
	"github.com/latebit-io/nib/tui/ui/textarea"
)

// ProjectOpenFileMsg is sent when the user selects a file in the project pane.
// AppModel catches this and opens the file in the editor.
type ProjectOpenFileMsg struct{ Path string }

// ProjectSetActiveGoalMsg is sent when the user activates a work item.
// AppModel handles the session mutation to keep writes in one place.
type ProjectSetActiveGoalMsg struct{ Title string }

// ProjectMarkGoalDoneMsg is sent when the user marks an active task as done.
// AppModel handles the session mutation.
type ProjectMarkGoalDoneMsg struct{ Title string }

// ProjectCreateFileMsg is sent when the user creates a new file via inline input.
// Path is relative to the project root.
type ProjectCreateFileMsg struct{ Path string }

// ProjectCreateDirMsg is sent when the user creates a new directory via inline input.
// Path is relative to the project root.
type ProjectCreateDirMsg struct{ Path string }

// ProjectDeleteFileMsg is sent when the user requests file deletion.
// Path is relative to the project root.
type ProjectDeleteFileMsg struct{ Path string }

// Package-level styles — allocated once, never in render paths.
var (
	projSectionStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("4"))

	projCursorStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("237"))

	projDirStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("12"))

	projFileStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	projDimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240"))

	projInputCursorStyle = lipgloss.NewStyle().Reverse(true)

	projBadgeModStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("3"))

	projWorkHeadingStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("252"))

	projTaskActiveStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("2")).
				Bold(true)

	projTaskDoneStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("240"))

	projTaskPendingStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("252"))
)

// projectSession is the narrow read-only slice of session that the project pane needs.
// Keeping this minimal prevents the TUI pane from coupling to the full session surface.
type projectSession interface {
	WorkTree() *project.Tree
	AgentModifiedFiles() []string
	ListFiles() ([]string, error)
	ListFilesAndDirs() (files []string, dirs []string, err error)
	ProjectRoot() string
}

// ProjectPaneModel implements Pane for the project view panel.
// Two sections: WORK (from project.md in demarkus) and FILES (project tree).
type ProjectPaneModel struct {
	session projectSession

	// Layout
	width  int
	height int

	// Tree data
	filesTree *TreeNode // full project file tree with ctx/mod badges

	// Work tree display state — collapse tracking is TUI-specific,
	// separate from the engine's project.Tree.
	// workExpanded tracks headings the user has explicitly expanded.
	// Headings not in the map default to collapsed; headings with
	// active descendants are auto-expanded on rebuild.
	workExpanded    map[string]bool // title → expanded (headings only)
	workDepthOffset int             // subtracted from node.Depth for rendering (root elision)

	// Flattened display items — rebuilt on refresh
	items []projectItem

	// Cursor and scroll
	cursorIdx    int
	scrollOffset int

	// Inline file/directory creation input
	creatingFile  bool               // true when inline input is active
	creatingDir   bool               // true when creating a directory (not a file)
	createInput   *textarea.TextArea // single-line name input
	createDirPath string             // parent directory (relative, forward slashes)

	// Empty directories created by the user — not returned by filelist.Walk,
	// so they must be injected into the tree during rebuild. Cleared once
	// they contain files (i.e., Walk returns a file inside them).
	emptyDirs map[string]bool // relative paths (forward slashes)

	// Delete confirmation
	confirmDelete     bool   // true when confirmation is showing
	confirmDeletePath string // relative path of file to delete

	// dirty is set when state changes while the pane is hidden.
	// rebuild() is deferred until the pane becomes visible.
	dirty bool

	// pendingReveal is set before rebuild to auto-expand ancestor
	// directories of a newly created file so it's visible in the tree.
	pendingReveal string
}

// projectItem is a flattened display row in the project pane.
type projectItem struct {
	node     *TreeNode     // non-nil for file items
	workNode *project.Node // non-nil for work items
	section  string        // "work" or "files"
	isHeader bool          // true for section headers
}

// NewProjectPaneModel creates the project pane.
func NewProjectPaneModel(sess projectSession) *ProjectPaneModel {
	p := &ProjectPaneModel{
		session:      sess,
		workExpanded: make(map[string]bool),
		createInput:  textarea.New(1),
	}
	p.rebuild()
	return p
}

// Title returns the project name for display in the pane border.
// Falls back to "Project" if no work tree is loaded.
// Implements the Titled interface.
func (p *ProjectPaneModel) Title() string {
	if tree := p.workTree(); tree != nil && tree.ProjectName != "" {
		return tree.ProjectName
	}
	return "Project"
}

// workTree returns the session's work tree, or nil if no session is set.
func (p *ProjectPaneModel) workTree() *project.Tree {
	if p.session == nil {
		return nil
	}
	return p.session.WorkTree()
}

// AddEmptyDir registers a directory that was just created and has no files.
// It will be injected into the tree during rebuild until files appear inside it.
func (p *ProjectPaneModel) AddEmptyDir(relPath string) {
	if p.emptyDirs == nil {
		p.emptyDirs = make(map[string]bool)
	}
	p.emptyDirs[filepath.ToSlash(relPath)] = true
}

// IsInputActive returns true when inline input or confirmation is active.
// AppModel uses this to avoid stealing keys.
func (p *ProjectPaneModel) IsInputActive() bool {
	return p.creatingFile || p.confirmDelete
}

// Update handles input when the project pane has focus.
func (p *ProjectPaneModel) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if p.confirmDelete {
			return p.handleConfirmDelete(msg)
		}
		if p.creatingFile {
			return p.handleCreateInput(msg)
		}
		return p.handleKey(msg)
	case tea.MouseClickMsg:
		return p.handleMouseClick(msg)
	case tea.MouseWheelMsg:
		return p.handleMouseWheel(msg)
	}
	return nil
}

// Render produces the project pane view.
func (p *ProjectPaneModel) Render() string {
	if p.width == 0 || p.height == 0 {
		return ""
	}

	var lines []string
	visible := p.visibleItems()

	// Reserve one row for the inline create input so we never exceed p.height.
	maxItemRows := p.height
	if p.creatingFile {
		maxItemRows--
	}

	for i, item := range visible {
		if len(lines) >= maxItemRows {
			break
		}
		globalIdx := p.scrollOffset + i
		line := p.renderItem(item, globalIdx == p.cursorIdx)
		lines = append(lines, line)

		// Insert inline create input after the cursor item.
		if p.creatingFile && globalIdx == p.cursorIdx {
			lines = append(lines, p.renderCreateInput())
		}
	}

	// Pad to height
	for len(lines) < p.height {
		lines = append(lines, strings.Repeat(" ", p.width))
	}

	// Overlay delete confirmation within the pane
	if p.confirmDelete {
		lines = p.overlayConfirmDelete(lines)
	}

	return strings.Join(lines, "\n")
}

// SetSize updates the pane dimensions.
func (p *ProjectPaneModel) SetSize(width, height int) {
	p.width = width
	p.height = height
	p.clampScroll()
}

// rebuild refreshes all tree data from the session.
func (p *ProjectPaneModel) rebuild() {
	// Capture state to restore after rebuild
	var selectedPath string
	if p.cursorIdx >= 0 && p.cursorIdx < len(p.items) && p.items[p.cursorIdx].node != nil {
		selectedPath = p.items[p.cursorIdx].node.Path
	}
	prevFilesExpanded := ExpandedPaths(p.filesTree)

	modifiedFiles := p.session.AgentModifiedFiles()

	// Build set for badge lookup — normalize to forward slashes
	// to match TreeNode.Path (BuildTree normalizes internally).
	modSet := toSlashSet(modifiedFiles)

	// FILES section — full file tree with badges
	projectFiles, projectDirs, err := p.session.ListFilesAndDirs()
	if err != nil && !errors.Is(err, filelist.ErrCapped) {
		slog.Error("project pane: failed to list files", "err", err)
		projectFiles = nil
	}
	p.filesTree = BuildTree(projectFiles)

	// Insert directories that exist on disk but have no files (empty or
	// all contents gitignored). BuildTree only creates dir nodes as parents
	// of files, so empty dirs must be inserted explicitly.
	for _, dir := range projectDirs {
		if findNode(p.filesTree, dir) == nil {
			InsertDir(p.filesTree, dir)
		}
	}
	// Also inject any dirs created via the TUI this session.
	for dir := range p.emptyDirs {
		if findNode(p.filesTree, dir) != nil {
			delete(p.emptyDirs, dir) // now tracked by Walk
		} else {
			InsertDir(p.filesTree, dir)
		}
	}
	sortChildren(p.filesTree)

	RestoreExpanded(p.filesTree, prevFilesExpanded)
	if p.pendingReveal != "" {
		ExpandToPath(p.filesTree, p.pendingReveal)
		p.pendingReveal = ""
	}
	SetBadges(p.filesTree, modSet)

	// Flatten into display items
	p.flattenItems()

	// Restore cursor to the same node, or clamp to bounds
	p.restoreCursor(selectedPath)
	p.clampScroll()
}

// restoreCursor finds the item with the given path and moves the cursor to it.
// Falls back to clamping within bounds if the path is not found.
func (p *ProjectPaneModel) restoreCursor(path string) {
	if path != "" {
		for i, item := range p.items {
			if item.node != nil && item.node.Path == path {
				p.cursorIdx = i
				return
			}
		}
	}
	if p.cursorIdx >= len(p.items) {
		p.cursorIdx = len(p.items) - 1
	}
	if p.cursorIdx < 0 {
		p.cursorIdx = 0
	}
}

// flattenItems builds the unified item list from WORK and FILES sections.
func (p *ProjectPaneModel) flattenItems() {
	p.items = nil

	// WORK section — from demarkus project.md
	if tree := p.workTree(); tree != nil && len(tree.Roots) > 0 {
		p.items = append(p.items, projectItem{isHeader: true, section: "work"})
		p.workDepthOffset = 0

		// Auto-expand headings that contain active tasks.
		p.autoExpandActive(tree.Roots)

		// If there's a single root whose title matches the project name
		// (already shown in the pane border), elide it and promote children.
		if len(tree.Roots) == 1 && tree.Roots[0].IsHeading && tree.Roots[0].Title == tree.ProjectName {
			p.workDepthOffset = 1
			for _, child := range tree.Roots[0].Children {
				p.flattenWorkNode(child, 0)
			}
		} else {
			for _, root := range tree.Roots {
				p.flattenWorkNode(root, 0)
			}
		}
	}

	// FILES section — full project file tree
	p.items = append(p.items, projectItem{isHeader: true, section: "files"})
	for _, n := range FlattenVisible(p.filesTree) {
		p.items = append(p.items, projectItem{node: n, section: "files"})
	}
}

// autoExpandActive expands headings that contain incomplete tasks,
// without overriding headings the user has explicitly toggled.
// A heading is "explicitly toggled" if it's in workExpanded; headings
// absent from the map get their state set here based on incomplete descendants.
func (p *ProjectPaneModel) autoExpandActive(roots []*project.Node) {
	for _, root := range roots {
		p.autoExpandNode(root)
	}
}

// autoExpandNode sets workExpanded for headings with incomplete descendants,
// skipping headings the user has already toggled (present in the map).
func (p *ProjectPaneModel) autoExpandNode(n *project.Node) bool {
	if !n.IsHeading {
		return n.Status != project.TaskDone
	}

	active := false
	for _, child := range n.Children {
		if p.autoExpandNode(child) {
			active = true
		}
	}

	// Only auto-set if the user hasn't explicitly toggled this heading.
	if _, toggled := p.workExpanded[n.Title]; !toggled && active {
		p.workExpanded[n.Title] = true
	}
	return active
}

// flattenWorkNode recursively flattens a work tree node into display items,
// respecting TUI-specific collapse state.
func (p *ProjectPaneModel) flattenWorkNode(n *project.Node, depth int) {
	p.items = append(p.items, projectItem{workNode: n, section: "work"})

	if n.IsHeading && !p.workExpanded[n.Title] {
		return // collapsed — skip children
	}
	for _, child := range n.Children {
		p.flattenWorkNode(child, depth+1)
	}
}

// handleKey processes key events when the pane has focus.
func (p *ProjectPaneModel) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.Code {
	case tea.KeyUp:
		return p.moveCursor(-1)
	case tea.KeyDown:
		return p.moveCursor(1)
	case tea.KeyEnter:
		return p.activateItem()
	}
	// Single-character vim-style navigation
	if rs := []rune(msg.Text); len(rs) == 1 {
		switch rs[0] {
		case 'k':
			return p.moveCursor(-1)
		case 'j':
			return p.moveCursor(1)
		case 'n':
			return p.startFileCreate()
		case 'f':
			return p.startDirCreate()
		case 'd':
			return p.startFileDelete()
		}
	}
	return nil
}

// handleMouseWheel processes mouse wheel events.
func (p *ProjectPaneModel) handleMouseWheel(msg tea.MouseWheelMsg) tea.Cmd {
	switch msg.Button {
	case tea.MouseWheelUp:
		p.scrollOffset -= 3
		p.clampScroll()
	case tea.MouseWheelDown:
		p.scrollOffset += 3
		p.clampScroll()
	}
	return nil
}

// handleMouseClick processes mouse click events.
func (p *ProjectPaneModel) handleMouseClick(msg tea.MouseClickMsg) tea.Cmd {
	if msg.Button != tea.MouseLeft {
		return nil
	}
	idx := p.scrollOffset + msg.Y
	if idx >= 0 && idx < len(p.items) {
		p.cursorIdx = idx
		return p.activateItem()
	}
	return nil
}

// moveCursor shifts the cursor by delta, skipping headers.
func (p *ProjectPaneModel) moveCursor(delta int) tea.Cmd {
	if len(p.items) == 0 {
		return nil
	}

	next := p.cursorIdx + delta
	// Skip headers
	for next >= 0 && next < len(p.items) && p.items[next].isHeader {
		next += delta
	}
	if next < 0 || next >= len(p.items) {
		return nil
	}
	p.cursorIdx = next
	p.ensureVisible()
	return nil
}

// activateItem handles Enter on the current item.
func (p *ProjectPaneModel) activateItem() tea.Cmd {
	if p.cursorIdx < 0 || p.cursorIdx >= len(p.items) {
		return nil
	}
	item := p.items[p.cursorIdx]
	if item.isHeader {
		return nil
	}

	// Work section — toggle headings, activate tasks
	if item.workNode != nil {
		return p.activateWorkItem(item.workNode)
	}

	// Files section — toggle directories, open files
	node := item.node
	if node.IsDir {
		node.Toggle()
		p.flattenItems()
		if p.cursorIdx >= len(p.items) {
			p.cursorIdx = len(p.items) - 1
		}
		p.clampScroll()
		return nil
	}
	// File — emit open message
	absPath := filepath.Join(p.session.ProjectRoot(), node.Path)
	return func() tea.Msg { return ProjectOpenFileMsg{Path: absPath} }
}

// activateWorkItem handles Enter on a work tree node.
// Headings toggle collapse; tasks cycle status: pending → active, active → done.
func (p *ProjectPaneModel) activateWorkItem(n *project.Node) tea.Cmd {
	if n.IsHeading {
		p.workExpanded[n.Title] = !p.workExpanded[n.Title]
		p.flattenItems()
		if p.cursorIdx >= len(p.items) {
			p.cursorIdx = len(p.items) - 1
		}
		p.clampScroll()
		return nil
	}
	title := n.Title
	switch n.Status {
	case project.TaskActive:
		// Active → done
		return func() tea.Msg { return ProjectMarkGoalDoneMsg{Title: title} }
	case project.TaskPending:
		// Pending → active
		return func() tea.Msg { return ProjectSetActiveGoalMsg{Title: title} }
	default:
		// Done tasks are final — no action on Enter
		return nil
	}
}

// startFileCreate activates inline input for creating a new file.
func (p *ProjectPaneModel) startFileCreate() tea.Cmd {
	return p.startCreate(false)
}

// startDirCreate activates inline input for creating a new directory.
func (p *ProjectPaneModel) startDirCreate() tea.Cmd {
	return p.startCreate(true)
}

// startCreate activates inline input for creating a new file or directory.
// The parent directory is derived from the cursor position: if on a directory,
// create inside it; if on a file, create in its parent directory.
func (p *ProjectPaneModel) startCreate(dir bool) tea.Cmd {
	if p.cursorIdx < 0 || p.cursorIdx >= len(p.items) {
		return nil
	}
	item := p.items[p.cursorIdx]
	if item.section != "files" {
		return nil
	}

	// FILES header or empty section → create at project root.
	if item.isHeader || item.node == nil {
		p.createDirPath = ""
	} else if item.node.IsDir {
		p.createDirPath = item.node.Path
		// Expand the directory so the input appears inside it.
		item.node.Collapsed = false
		p.flattenItems()
		p.restoreCursor("")
		p.clampScroll()
	} else {
		// Use parent directory — Path uses forward slashes.
		p.createDirPath = path.Dir(item.node.Path)
		if p.createDirPath == "." {
			p.createDirPath = ""
		}
	}

	p.creatingFile = true
	p.creatingDir = dir
	p.createInput.Reset()
	p.createInput.SetSize(p.width)
	return nil
}

// startFileDelete shows an inline delete confirmation for the file or directory
// under cursor. Only works in the FILES section (not headers or work items).
func (p *ProjectPaneModel) startFileDelete() tea.Cmd {
	if p.cursorIdx < 0 || p.cursorIdx >= len(p.items) {
		return nil
	}
	item := p.items[p.cursorIdx]
	if item.section != "files" || item.isHeader || item.node == nil {
		return nil
	}
	p.confirmDelete = true
	p.confirmDeletePath = item.node.Path
	return nil
}

// handleCreateInput delegates key events to the inline file creation TextArea.
// Submit creates the file; cancel dismisses the input.
func (p *ProjectPaneModel) handleCreateInput(msg tea.KeyPressMsg) tea.Cmd {
	cmd := p.createInput.Update(msg)
	if cmd == nil {
		return nil
	}

	result := cmd()
	switch result.(type) {
	case textarea.SubmitMsg:
		name := strings.TrimSpace(p.createInput.Content())
		isDir := p.creatingDir
		p.creatingFile = false
		p.creatingDir = false
		if name == "" {
			return nil
		}
		relPath := name
		if p.createDirPath != "" {
			relPath = p.createDirPath + "/" + name
		}
		if isDir {
			return func() tea.Msg { return ProjectCreateDirMsg{Path: relPath} }
		}
		return func() tea.Msg { return ProjectCreateFileMsg{Path: relPath} }
	case textarea.CancelMsg:
		p.creatingFile = false
		p.creatingDir = false
		return nil
	default:
		return func() tea.Msg { return result }
	}
}

// handleConfirmDelete processes keys during delete confirmation.
// Enter/y confirms, Escape/n cancels.
func (p *ProjectPaneModel) handleConfirmDelete(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.Code {
	case tea.KeyEnter:
		return p.confirmDeleteAction()
	case tea.KeyEscape:
		return p.cancelDeleteAction()
	}
	if rs := []rune(msg.Text); len(rs) == 1 {
		switch rs[0] {
		case 'y', 'Y':
			return p.confirmDeleteAction()
		case 'n', 'N':
			return p.cancelDeleteAction()
		}
	}
	return nil
}

func (p *ProjectPaneModel) confirmDeleteAction() tea.Cmd {
	deletePath := p.confirmDeletePath
	p.confirmDelete = false
	p.confirmDeletePath = ""
	return func() tea.Msg { return ProjectDeleteFileMsg{Path: deletePath} }
}

func (p *ProjectPaneModel) cancelDeleteAction() tea.Cmd {
	p.confirmDelete = false
	p.confirmDeletePath = ""
	return nil
}

// visibleItems returns the items in the current scroll viewport.
func (p *ProjectPaneModel) visibleItems() []projectItem {
	if len(p.items) == 0 {
		return nil
	}
	start := p.scrollOffset
	end := start + p.height
	if end > len(p.items) {
		end = len(p.items)
	}
	return p.items[start:end]
}

// ensureVisible adjusts scroll so the cursor is in the viewport.
func (p *ProjectPaneModel) ensureVisible() {
	if p.cursorIdx < p.scrollOffset {
		p.scrollOffset = p.cursorIdx
	}
	if p.cursorIdx >= p.scrollOffset+p.height {
		p.scrollOffset = p.cursorIdx - p.height + 1
	}
	p.clampScroll()
}

// clampScroll ensures scroll offset is within valid range.
func (p *ProjectPaneModel) clampScroll() {
	maxScroll := len(p.items) - p.height
	if maxScroll < 0 {
		maxScroll = 0
	}
	if p.scrollOffset > maxScroll {
		p.scrollOffset = maxScroll
	}
	if p.scrollOffset < 0 {
		p.scrollOffset = 0
	}
}

// renderItem renders a single display row.
func (p *ProjectPaneModel) renderItem(item projectItem, selected bool) string {
	if item.isHeader {
		return p.renderHeader(item.section, selected)
	}
	if item.workNode != nil {
		return p.renderWorkNode(item.workNode, selected)
	}
	return p.renderFileNode(item, selected)
}

// renderHeader renders a section header.
func (p *ProjectPaneModel) renderHeader(section string, selected bool) string {
	label := strings.ToUpper(section)
	text := projSectionStyle.Render(label)

	// Pad to width
	line := clampToWidth(text, p.width)
	if selected {
		line = projCursorStyle.Render(line)
	}
	return line
}

// renderWorkNode renders a work tree node (heading or task).
func (p *ProjectPaneModel) renderWorkNode(n *project.Node, selected bool) string {
	depth := n.Depth - p.workDepthOffset
	if depth < 0 {
		depth = 0
	}
	indent := strings.Repeat("  ", depth)

	var icon, name string
	if n.IsHeading {
		if p.workExpanded[n.Title] {
			icon = "▾ "
		} else {
			icon = "▸ "
		}
		name = projWorkHeadingStyle.Render(n.Title)
	} else {
		icon, name = workTaskIconAndName(n)
	}

	left := indent + icon + name
	line := clampToWidth(left, p.width)
	if selected {
		line = projCursorStyle.Render(line)
	}
	return line
}

// workTaskIconAndName returns the status icon and styled name for a task node.
func workTaskIconAndName(n *project.Node) (string, string) {
	switch n.Status {
	case project.TaskActive:
		return "● ", projTaskActiveStyle.Render(n.Title)
	case project.TaskDone:
		return "✓ ", projTaskDoneStyle.Render(n.Title)
	default:
		return "○ ", projTaskPendingStyle.Render(n.Title)
	}
}

// renderFileNode renders a file or directory tree node.
func (p *ProjectPaneModel) renderFileNode(item projectItem, selected bool) string {
	node := item.node
	depth := node.Depth()

	// Indentation
	indent := strings.Repeat("  ", depth)

	// Icon
	var icon string
	if node.IsDir {
		if node.Collapsed {
			icon = "▸ "
		} else {
			icon = "▾ "
		}
	} else {
		icon = "  "
	}

	// Name with style
	var name string
	if node.IsDir {
		name = projDirStyle.Render(node.Name + "/")
	} else if node.Badge != "" {
		name = projFileStyle.Render(node.Name)
	} else {
		name = projDimStyle.Render(node.Name)
	}

	// Badge
	badge := p.renderBadge(node.Badge)

	// Assemble: indent + icon + name + padding + badge
	left := indent + icon + name
	line := assembleLine(left, badge, p.width)

	if selected {
		line = projCursorStyle.Render(line)
	}
	return line
}

// renderCreateInput renders the inline file creation input row.
// Shows the directory prefix and the editable filename with a cursor.
func (p *ProjectPaneModel) renderCreateInput() string {
	// Determine indentation depth from the target directory.
	depth := 0
	if p.createDirPath != "" {
		if n := findNode(p.filesTree, p.createDirPath); n != nil {
			depth = n.Depth() + 1
		}
	}

	indent := strings.Repeat("  ", depth)
	prefix := indent + "  " // align with file names (under the icon column)

	rendered := p.createInput.Render()
	var content string
	if len(rendered) > 0 {
		content = rendered[0].Text
	}

	// Render character-by-character to show cursor.
	cursorRow, cursorCol := p.createInput.CursorPosition()
	runes := []rune(content)
	var lineBuilder strings.Builder
	lineBuilder.WriteString(prefix)
	prefixW := lipgloss.Width(prefix)
	cellsUsed := prefixW

	for j, r := range runes {
		ch := string(r)
		w := lipgloss.Width(ch)
		if cellsUsed+w > p.width {
			break
		}
		if cursorRow == 0 && j == cursorCol {
			lineBuilder.WriteString(projInputCursorStyle.Render(ch))
		} else {
			lineBuilder.WriteString(ch)
		}
		cellsUsed += w
	}
	// Cursor at end of content.
	if cursorRow == 0 && cursorCol >= len(runes) && cellsUsed < p.width {
		lineBuilder.WriteString(projInputCursorStyle.Render(" "))
		cellsUsed++
	}
	// Pad to width.
	if cellsUsed < p.width {
		lineBuilder.WriteString(strings.Repeat(" ", p.width-cellsUsed))
	}

	return projFileStyle.Render(lineBuilder.String())
}

// overlayConfirmDelete renders a small confirmation prompt centered within
// the pane, overlaid on top of the existing lines.
func (p *ProjectPaneModel) overlayConfirmDelete(lines []string) []string {
	name := filepath.Base(p.confirmDeletePath)
	prompt := " Delete " + name + "? (y/n) "

	if lipgloss.Width(prompt) > p.width {
		prompt = " Delete? (y/n) "
	}

	styled := projCursorStyle.Bold(true).Render(prompt)

	// Center vertically and horizontally
	row := p.height / 2
	if row >= len(lines) {
		row = 0
	}
	lines[row] = lipgloss.Place(p.width, 1, lipgloss.Center, lipgloss.Center, styled)
	return lines
}

// renderBadge renders a badge string with appropriate styling.
func (p *ProjectPaneModel) renderBadge(badge string) string {
	if badge == "" {
		return ""
	}
	if badge == "mod" {
		return projBadgeModStyle.Render("mod")
	}
	return badge
}

// assembleLine combines left content and right badge, padding in between.
// Truncates left content (with a trailing "…") if it would exceed the pane
// width so the developer reads the clip as intentional rather than as a
// broken line.
func assembleLine(left, right string, width int) string {
	leftW := lipgloss.Width(left)
	rightW := lipgloss.Width(right)

	if right == "" {
		return clampToWidth(left, width)
	}

	// Truncate left to make room for badge
	maxLeft := width - rightW - 2 // 1 space + at least 1 char gap
	if maxLeft < 1 {
		return clampToWidth(left, width)
	}
	if leftW > maxLeft {
		left = ansi.Truncate(left, maxLeft, "…")
		leftW = lipgloss.Width(left)
	}

	gap := width - leftW - rightW - 1
	if gap < 1 {
		return clampToWidth(left, width)
	}
	return fmt.Sprintf("%s%s %s", left, strings.Repeat(" ", gap), right)
}

// clampToWidth truncates or pads a string to exactly the given width.
// Appends "…" when truncating so the clip reads as intentional.
// Uses ANSI-aware measurement so styled strings are handled correctly.
func clampToWidth(s string, width int) string {
	w := lipgloss.Width(s)
	if w > width {
		return ansi.Truncate(s, width, "…")
	}
	if w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

// toSlashSet converts a string slice to a set map, normalizing paths
// to forward slashes so they match TreeNode.Path values.
func toSlashSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[filepath.ToSlash(s)] = true
	}
	return m
}
