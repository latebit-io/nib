package ui

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/latebit-io/junto/engine/filelist"
	"github.com/latebit-io/junto/engine/session"
)

// ProjectOpenFileMsg is sent when the user selects a file in the project pane.
// AppModel catches this and opens the file in the editor.
type ProjectOpenFileMsg struct{ Path string }

// ProjectRefreshMsg triggers a rebuild of the project pane's tree data.
type ProjectRefreshMsg struct{}

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
)

// ProjectPaneModel implements Pane for the project view panel.
type ProjectPaneModel struct {
	session *session.Session

	// Layout
	width  int
	height int

	// Tree data
	contextTree  *TreeNode
	modifiedTree *TreeNode
	projectTree  *TreeNode

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
	node     *TreeNode
	section  string // "context", "review", "modified", "project"
	isHeader bool   // true for section headers
}

// NewProjectPaneModel creates the project pane.
func NewProjectPaneModel(sess *session.Session) *ProjectPaneModel {
	p := &ProjectPaneModel{
		session: sess,
	}
	p.rebuild()
	return p
}

// Update handles input when the project pane has focus.
func (p *ProjectPaneModel) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case ProjectRefreshMsg:
		p.rebuild()
		return nil
	case tea.KeyMsg:
		return p.handleKey(msg)
	case tea.MouseMsg:
		return p.handleMouse(msg)
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
	prevProjectExpanded := ExpandedPaths(p.projectTree)

	contextFiles := p.session.ContextFiles()
	modifiedFiles := p.session.AgentModifiedFiles()

	// Build sets for badge lookup
	ctxSet := toSet(contextFiles)
	modSet := toSet(modifiedFiles)

	// Context section — flat list of context files
	p.contextTree = BuildTree(contextFiles)
	expandAll(p.contextTree)

	// Modified section — files modified by agent
	p.modifiedTree = BuildTree(modifiedFiles)
	expandAll(p.modifiedTree)

	// Project section — full file tree, collapsed by default then restore
	projectFiles, err := p.session.ListFiles()
	if err != nil && !errors.Is(err, filelist.ErrCapped) {
		slog.Error("project pane: failed to list files", "err", err)
		projectFiles = nil
	}
	p.projectTree = BuildTree(projectFiles)
	RestoreExpanded(p.projectTree, prevProjectExpanded)
	SetBadges(p.projectTree, ctxSet, modSet)

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

// flattenItems builds the unified item list from all sections.
func (p *ProjectPaneModel) flattenItems() {
	p.items = nil

	// CONTEXT
	p.items = append(p.items, projectItem{isHeader: true, section: "context"})
	for _, n := range FlattenVisible(p.contextTree) {
		p.items = append(p.items, projectItem{node: n, section: "context"})
	}

	// REVIEW (placeholder)
	p.items = append(p.items, projectItem{isHeader: true, section: "review"})

	// MODIFIED
	p.items = append(p.items, projectItem{isHeader: true, section: "modified"})
	for _, n := range FlattenVisible(p.modifiedTree) {
		p.items = append(p.items, projectItem{node: n, section: "modified"})
	}

	// PROJECT
	p.items = append(p.items, projectItem{isHeader: true, section: "project"})
	for _, n := range FlattenVisible(p.projectTree) {
		p.items = append(p.items, projectItem{node: n, section: "project"})
	}
}

// handleKey processes key events when the pane has focus.
func (p *ProjectPaneModel) handleKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyUp:
		return p.moveCursor(-1)
	case tea.KeyDown:
		return p.moveCursor(1)
	case tea.KeyEnter:
		return p.activateItem()
	case tea.KeyRunes:
		if len(msg.Runes) == 1 {
			switch msg.Runes[0] {
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
	}
	return nil
}

// handleMouse processes mouse events.
func (p *ProjectPaneModel) handleMouse(msg tea.MouseMsg) tea.Cmd {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		p.scrollOffset -= 3
		p.clampScroll()
		return nil
	case tea.MouseButtonWheelDown:
		p.scrollOffset += 3
		p.clampScroll()
		return nil
	case tea.MouseButtonLeft:
		if msg.Action == tea.MouseActionRelease {
			return nil
		}
		idx := p.scrollOffset + msg.Y
		if idx >= 0 && idx < len(p.items) {
			p.cursorIdx = idx
			return p.activateItem()
		}
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

// addContext adds the file under cursor to the agent context set.
func (p *ProjectPaneModel) addContext() tea.Cmd {
	if p.cursorIdx < 0 || p.cursorIdx >= len(p.items) {
		return nil
	}
	item := p.items[p.cursorIdx]
	if item.isHeader || item.node == nil || item.node.IsDir {
		return nil
	}
	absPath := filepath.Join(p.session.ProjectRoot(), item.node.Path)
	p.session.AddContext(absPath)
	p.rebuild()
	return nil
}

// removeContext removes the file under cursor from the agent context set.
func (p *ProjectPaneModel) removeContext() tea.Cmd {
	if p.cursorIdx < 0 || p.cursorIdx >= len(p.items) {
		return nil
	}
	item := p.items[p.cursorIdx]
	if item.isHeader || item.node == nil || item.node.IsDir {
		return nil
	}
	absPath := filepath.Join(p.session.ProjectRoot(), item.node.Path)
	p.session.RemoveContext(absPath)
	p.rebuild()
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
	return p.renderNode(item, selected)
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

// renderNode renders a file or directory tree node.
func (p *ProjectPaneModel) renderNode(item projectItem, selected bool) string {
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
		left = lipgloss.NewStyle().MaxWidth(maxLeft).Render(left)
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
		return lipgloss.NewStyle().MaxWidth(width).Render(s)
	}
	if w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

// expandAll recursively expands all directory nodes.
func expandAll(root *TreeNode) {
	if root == nil {
		return
	}
	walkTree(root, func(n *TreeNode) {
		if n.IsDir {
			n.Collapsed = false
		}
	})
}

// toSet converts a string slice to a set map.
func toSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}
