package ui

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/latebit-io/junto/engine/filelist"
	"github.com/latebit-io/junto/engine/project"
)

// ProjectOpenFileMsg is sent when the user selects a file in the project pane.
// AppModel catches this and opens the file in the editor.
type ProjectOpenFileMsg struct{ Path string }

// ProjectAddContextMsg is sent when the user adds a file to the context set.
// AppModel handles this to keep all session mutations in one place.
type ProjectAddContextMsg struct{ Path string }

// ProjectRemoveContextMsg is sent when the user removes a file from the context set.
type ProjectRemoveContextMsg struct{ Path string }

// ProjectSetActiveGoalMsg is sent when the user activates a work item.
// AppModel handles the session mutation to keep writes in one place.
type ProjectSetActiveGoalMsg struct{ Title string }

// ProjectMarkGoalDoneMsg is sent when the user marks an active task as done.
// AppModel handles the session mutation.
type ProjectMarkGoalDoneMsg struct{ Title string }

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

	projBadgeCtxStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("2"))

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

	// projClampStyle is a zero-value base style used by clampToWidth and
	// assembleLine.  Calling .MaxWidth(w) on it is cheaper than NewStyle()
	// because lipgloss styles are value types — the copy reuses internal
	// storage instead of allocating from scratch.
	projClampStyle = lipgloss.NewStyle()
)

// projectSession is the narrow read-only slice of session that the project pane needs.
// Keeping this minimal prevents the TUI pane from coupling to the full session surface.
type projectSession interface {
	WorkTree() *project.Tree
	ContextFiles() []string
	AgentModifiedFiles() []string
	ListFiles() ([]string, error)
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

	// dirty is set when state changes while the pane is hidden.
	// rebuild() is deferred until the pane becomes visible.
	dirty bool
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

// Update handles input when the project pane has focus.
func (p *ProjectPaneModel) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
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

	for i, item := range visible {
		globalIdx := p.scrollOffset + i
		line := p.renderItem(item, globalIdx == p.cursorIdx)
		lines = append(lines, line)
	}

	// Pad to height
	for len(lines) < p.height {
		lines = append(lines, strings.Repeat(" ", p.width))
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

	contextFiles := p.session.ContextFiles()
	modifiedFiles := p.session.AgentModifiedFiles()

	// Build sets for badge lookup — normalize to forward slashes
	// to match TreeNode.Path (BuildTree normalizes internally).
	ctxSet := toSlashSet(contextFiles)
	modSet := toSlashSet(modifiedFiles)

	// FILES section — full file tree with badges
	projectFiles, err := p.session.ListFiles()
	if err != nil && !errors.Is(err, filelist.ErrCapped) {
		slog.Error("project pane: failed to list files", "err", err)
		projectFiles = nil
	}
	p.filesTree = BuildTree(projectFiles)
	RestoreExpanded(p.filesTree, prevFilesExpanded)
	SetBadges(p.filesTree, ctxSet, modSet)

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
		case 'a':
			return p.addContext()
		case 'x':
			return p.removeContext()
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

// addContext emits a message to add the file under cursor to the context set.
// AppModel handles the actual session mutation.
func (p *ProjectPaneModel) addContext() tea.Cmd {
	if p.cursorIdx < 0 || p.cursorIdx >= len(p.items) {
		return nil
	}
	item := p.items[p.cursorIdx]
	if item.section != "files" || item.isHeader || item.node == nil || item.node.IsDir {
		return nil
	}
	absPath := filepath.Join(p.session.ProjectRoot(), item.node.Path)
	return func() tea.Msg { return ProjectAddContextMsg{Path: absPath} }
}

// removeContext emits a message to remove the file under cursor from the context set.
// AppModel handles the actual session mutation.
func (p *ProjectPaneModel) removeContext() tea.Cmd {
	if p.cursorIdx < 0 || p.cursorIdx >= len(p.items) {
		return nil
	}
	item := p.items[p.cursorIdx]
	if item.section != "files" || item.isHeader || item.node == nil || item.node.IsDir {
		return nil
	}
	absPath := filepath.Join(p.session.ProjectRoot(), item.node.Path)
	return func() tea.Msg { return ProjectRemoveContextMsg{Path: absPath} }
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

// renderBadge renders a badge string with appropriate styling.
func (p *ProjectPaneModel) renderBadge(badge string) string {
	if badge == "" {
		return ""
	}
	var parts []string
	for _, b := range strings.Fields(badge) {
		switch b {
		case "ctx":
			parts = append(parts, projBadgeCtxStyle.Render("ctx"))
		case "mod":
			parts = append(parts, projBadgeModStyle.Render("mod"))
		default:
			parts = append(parts, b)
		}
	}
	return strings.Join(parts, " ")
}

// assembleLine combines left content and right badge, padding in between.
// Truncates left content if it would exceed the pane width.
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
		left = projClampStyle.MaxWidth(maxLeft).Render(left)
		leftW = lipgloss.Width(left)
	}

	gap := width - leftW - rightW - 1
	if gap < 1 {
		return clampToWidth(left, width)
	}
	return fmt.Sprintf("%s%s %s", left, strings.Repeat(" ", gap), right)
}

// clampToWidth truncates or pads a string to exactly the given width.
// Uses ANSI-aware measurement so styled strings are handled correctly.
func clampToWidth(s string, width int) string {
	w := lipgloss.Width(s)
	if w > width {
		return projClampStyle.MaxWidth(width).Render(s)
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
