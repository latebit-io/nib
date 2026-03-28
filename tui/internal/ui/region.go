package ui

import (
	"math"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Package-level styles and colors for borders and dividers.
var (
	dividerDimColor   = lipgloss.Color("240")
	dividerFocusColor = lipgloss.Color("2")

	dividerDimStyle = lipgloss.NewStyle().
			Foreground(dividerDimColor).
			Background(lipgloss.Color("235"))

	dividerFocusStyle = lipgloss.NewStyle().
				Foreground(dividerFocusColor).
				Background(lipgloss.Color("235"))
)

// LayoutDirection determines how regions are arranged.
type LayoutDirection int

const (
	// Horizontal arranges panes side by side with vertical dividers.
	Horizontal LayoutDirection = iota
	// Vertical stacks panes top to bottom with horizontal dividers.
	Vertical
)

// MinPaneWidth is the minimum width a pane must have to remain visible.
const MinPaneWidth = 20

// Region is a named slot in the layout that holds a Pane.
type Region struct {
	Name    string
	Pane    Pane
	Ratio   float64 // proportion of available space (0.0-1.0)
	Visible bool    // user intent — Show/Hide toggle

	// Calculated by SetSize — position in global coordinates.
	x, y, width, height int
	// collapsed is set by recalculate when there isn't enough space.
	// Unlike Visible, this is transient and recalculated on every resize.
	collapsed bool
}

// RegionManager handles layout calculation, focus management,
// view composition, and mouse hit testing for all registered panes.
// It owns the spatial arrangement — AppModel delegates to it.
type RegionManager struct {
	Regions   []*Region
	Direction LayoutDirection
	FocusIdx  int
	Width     int
	Height    int

	// Drag state for divider resizing.
	dragging    bool
	dragDivider int   // index into visible regions: divider between [i] and [i+1]
	dragStartX  int   // global X at drag start
	dragStartW  []int // pane widths at drag start
}

// NewRegionManager creates a region manager with the given layout direction.
func NewRegionManager(dir LayoutDirection) *RegionManager {
	return &RegionManager{Direction: dir}
}

// Add registers a pane in a named region with a proportional size.
func (rm *RegionManager) Add(name string, pane Pane, ratio float64) {
	rm.Regions = append(rm.Regions, &Region{
		Name:    name,
		Pane:    pane,
		Ratio:   ratio,
		Visible: true,
	})
}

// Show makes a region visible and recalculates layout.
func (rm *RegionManager) Show(name string) {
	for _, r := range rm.Regions {
		if r.Name == name {
			r.Visible = true
			rm.recalculate()
			return
		}
	}
}

// Hide makes a region invisible and recalculates layout.
func (rm *RegionManager) Hide(name string) {
	for _, r := range rm.Regions {
		if r.Name == name {
			r.Visible = false
			rm.recalculate()
			return
		}
	}
}

// ReplacePane swaps the pane for a named region and re-applies its size.
func (rm *RegionManager) ReplacePane(name string, pane Pane) {
	for _, r := range rm.Regions {
		if r.Name == name {
			r.Pane = pane
			pane.SetSize(r.width, r.height)
			return
		}
	}
}

// SetSize updates the total available size and recalculates all regions.
func (rm *RegionManager) SetSize(w, h int) {
	rm.Width = w
	rm.Height = h
	rm.recalculate()
}

// FocusedPane returns the pane that currently has focus, or nil.
func (rm *RegionManager) FocusedPane() Pane {
	visible := rm.visibleRegions()
	if len(visible) == 0 {
		return nil
	}
	if rm.FocusIdx >= len(visible) {
		rm.FocusIdx = 0
	}
	return visible[rm.FocusIdx].Pane
}

// FocusedRegion returns the region that currently has focus, or nil.
func (rm *RegionManager) FocusedRegion() *Region {
	visible := rm.visibleRegions()
	if len(visible) == 0 {
		return nil
	}
	if rm.FocusIdx >= len(visible) {
		rm.FocusIdx = 0
	}
	return visible[rm.FocusIdx]
}

// FocusNext cycles focus to the next visible region.
func (rm *RegionManager) FocusNext() {
	visible := rm.visibleRegions()
	if len(visible) <= 1 {
		return
	}
	rm.FocusIdx = (rm.FocusIdx + 1) % len(visible)
}

// FocusByName sets focus to the region with the given name.
func (rm *RegionManager) FocusByName(name string) {
	for i, r := range rm.visibleRegions() {
		if r.Name == name {
			rm.FocusIdx = i
			return
		}
	}
}

// regionByName returns the region with the given name, or nil.
func (rm *RegionManager) regionByName(name string) *Region {
	for _, r := range rm.Regions {
		if r.Name == name {
			return r
		}
	}
	return nil
}

// RegionAt returns the region at global coordinates (x, y) and the
// local offsets within that region. Returns nil if no region is hit.
func (rm *RegionManager) RegionAt(x, y int) (*Region, int, int) {
	for _, r := range rm.visibleRegions() {
		if x >= r.x && x < r.x+r.width && y >= r.y && y < r.y+r.height {
			return r, x - r.x, y - r.y
		}
	}
	return nil, 0, 0
}

// HandleMouse translates global mouse coordinates to pane-local coordinates,
// sets focus on the clicked region, and forwards the event to the pane.
// Also handles divider dragging for pane resizing.
func (rm *RegionManager) HandleMouse(msg tea.MouseMsg) tea.Cmd {
	// Handle drag continuation and release
	if rm.dragging {
		switch msg.Action {
		case tea.MouseActionRelease:
			rm.dragging = false
			return nil
		case tea.MouseActionMotion:
			rm.handleDividerDrag(msg.X)
			return nil
		}
	}

	// Check for divider click to start drag (reject clicks outside pane area)
	if msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress && msg.Y >= 0 && msg.Y < rm.Height {
		if divIdx := rm.dividerAt(msg.X); divIdx >= 0 {
			rm.startDividerDrag(divIdx, msg.X)
			return nil
		}
	}

	region, localX, localY := rm.RegionAt(msg.X, msg.Y)
	if region == nil {
		return nil
	}

	// Set focus to clicked region
	rm.FocusByName(region.Name)

	// Forward mouse with translated coordinates
	localMsg := msg
	localMsg.X = localX
	localMsg.Y = localY
	return region.Pane.Update(localMsg)
}

// dividerAt returns the index of the divider at global X, or -1.
// In island layout, the draggable zone is the 2 border chars between
// adjacent panes (right border of pane i + left border of pane i+1).
func (rm *RegionManager) dividerAt(x int) int {
	if rm.Direction != Horizontal {
		return -1
	}
	visible := rm.visibleRegions()
	for i := 0; i < len(visible)-1; i++ {
		// Zone: right border of pane i to left border of pane i+1 (2 chars).
		zoneStart := visible[i].x + visible[i].width // right border
		zoneEnd := visible[i+1].x                    // left border (exclusive)
		if x >= zoneStart && x < zoneEnd {
			return i
		}
	}
	return -1
}

// startDividerDrag begins a divider drag operation, capturing initial state.
func (rm *RegionManager) startDividerDrag(divIdx, startX int) {
	visible := rm.visibleRegions()
	rm.dragging = true
	rm.dragDivider = divIdx
	rm.dragStartX = startX
	rm.dragStartW = make([]int, len(visible))
	for i, r := range visible {
		rm.dragStartW[i] = r.width
	}
}

// handleDividerDrag adjusts pane widths based on mouse position during drag.
func (rm *RegionManager) handleDividerDrag(currentX int) {
	visible := rm.visibleRegions()
	if rm.dragDivider >= len(visible)-1 {
		return
	}

	delta := currentX - rm.dragStartX
	left := rm.dragDivider
	right := rm.dragDivider + 1

	newLeftW := rm.dragStartW[left] + delta
	newRightW := rm.dragStartW[right] - delta

	// Enforce minimum widths. Both panes being below minimum simultaneously
	// is impossible — recalcHorizontal collapses panes that can't meet
	// MinPaneWidth, so visible panes always have width >= MinPaneWidth.
	if newLeftW < MinPaneWidth {
		newLeftW = MinPaneWidth
		newRightW = rm.dragStartW[left] + rm.dragStartW[right] - MinPaneWidth
	}
	if newRightW < MinPaneWidth {
		newRightW = MinPaneWidth
		newLeftW = rm.dragStartW[left] + rm.dragStartW[right] - MinPaneWidth
	}

	visible[left].width = newLeftW
	visible[right].width = newRightW

	// Update x positions for all panes from the left pane onward.
	// Island layout: content x = border-start + 1 (left border char).
	// Between panes: right border (1) + left border (1) = 2 chars.
	x := visible[left].x
	for i := left; i < len(visible); i++ {
		visible[i].x = x
		visible[i].Pane.SetSize(visible[i].width, visible[i].height)
		x += visible[i].width + 2 // +2 for right border + next left border
	}

	// Update ratios to reflect new proportions
	rm.updateRatios(visible)
}

// updateRatios recalculates ratios from current widths so future
// recalculations (e.g. terminal resize) preserve the user's layout.
func (rm *RegionManager) updateRatios(visible []*Region) {
	// Island layout: 2 border chars per pane, no gap.
	n := len(visible)
	overhead := 0
	if n > 1 {
		overhead = 2 * n
	}
	available := rm.Width - overhead
	if available <= 0 {
		return
	}
	totalExtra := available - len(visible)*MinPaneWidth
	if totalExtra <= 0 {
		for _, r := range visible {
			r.Ratio = 1.0 / float64(len(visible))
		}
		return
	}
	for _, r := range visible {
		w := r.width - MinPaneWidth
		if w < 0 {
			w = 0
		}
		r.Ratio = float64(w) / float64(totalExtra)
	}
	// Normalize so ratios sum to 1.0
	total := 0.0
	for _, r := range visible {
		total += r.Ratio
	}
	if total > 0 {
		for _, r := range visible {
			r.Ratio /= total
			// Round to avoid floating point drift
			r.Ratio = math.Round(r.Ratio*1000) / 1000
		}
	}
}

// Render composes all visible pane renders with dividers between them.
// Returns exactly rm.Height lines joined by newlines.
func (rm *RegionManager) Render() string {
	visible := rm.visibleRegions()
	if len(visible) == 0 || rm.Height == 0 {
		return ""
	}

	if len(visible) == 1 {
		lines := strings.Split(visible[0].Pane.Render(), "\n")
		for len(lines) < rm.Height {
			lines = append(lines, "")
		}
		if len(lines) > rm.Height {
			lines = lines[:rm.Height]
		}
		return strings.Join(lines, "\n")
	}

	// Determine which dividers are adjacent to the focused pane
	focusedRegion := rm.FocusedRegion()

	// Render each pane and normalize to its allocated height
	paneLines := make([][]string, len(visible))
	for i, r := range visible {
		lines := strings.Split(r.Pane.Render(), "\n")
		h := r.height
		for len(lines) < h {
			lines = append(lines, "")
		}
		if len(lines) > h {
			lines = lines[:h]
		}
		paneLines[i] = lines
	}

	if rm.Direction == Vertical {
		var lines []string
		for i, pl := range paneLines {
			lines = append(lines, pl...)
			if i < len(paneLines)-1 {
				style := rm.dividerStyleFor(visible, i, focusedRegion)
				lines = append(lines, style.Render(strings.Repeat("─", rm.Width)))
			}
		}
		for len(lines) < rm.Height {
			lines = append(lines, "")
		}
		if len(lines) > rm.Height {
			lines = lines[:rm.Height]
		}
		return strings.Join(lines, "\n")
	}

	// Horizontal layout: island style — each pane gets its own full rounded
	// border with a 1-char gap between panes (like JetBrains IDE panels).
	bordered := make([]string, len(visible))
	for i := range visible {
		content := strings.Join(paneLines[i], "\n")

		borderColor := dividerDimColor
		if visible[i] == focusedRegion {
			borderColor = dividerFocusColor
		}

		rendered := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(borderColor).
			Width(visible[i].width).
			Height(visible[i].height).
			Render(content)

		// Embed pane title in the top border if the pane implements Titled.
		if titled, ok := visible[i].Pane.(Titled); ok {
			if title := titled.Title(); title != "" {
				rendered = embedBorderTitle(rendered, title, borderColor, visible[i].width)
			}
		}

		bordered[i] = rendered
	}

	return lipgloss.JoinHorizontal(lipgloss.Top, bordered...)
}

// embedBorderTitle rebuilds the top border line with a title embedded.
// Builds from scratch to avoid slicing into ANSI escape sequences.
// Produces: ╭─ title ───────╮
func embedBorderTitle(rendered string, title string, borderColor lipgloss.Color, contentWidth int) string {
	lines := strings.SplitN(rendered, "\n", 2)
	if len(lines) < 2 {
		return rendered
	}

	totalWidth := contentWidth + 2 // content + left/right border chars
	label := " " + title + " "
	labelLen := len([]rune(label))

	// Need at least: ╭(1) + ─(1) + label + ─(1) + ╮(1)
	if totalWidth < labelLen+4 {
		return rendered
	}

	titleStyle := lipgloss.NewStyle().Foreground(borderColor).Bold(true)
	borderStyle := lipgloss.NewStyle().Foreground(borderColor)

	var top strings.Builder
	top.WriteString(borderStyle.Render("╭─"))
	top.WriteString(titleStyle.Render(label))
	dashesAfter := totalWidth - 2 - labelLen - 1 // after label, before ╮
	if dashesAfter > 0 {
		top.WriteString(borderStyle.Render(strings.Repeat("─", dashesAfter)))
	}
	top.WriteString(borderStyle.Render("╮"))

	return top.String() + "\n" + lines[1]
}

// dividerStyleFor returns the style for the divider between visible[i] and visible[i+1].
// If the focused region is on either side, the divider is green.
func (rm *RegionManager) dividerStyleFor(visible []*Region, i int, focused *Region) lipgloss.Style {
	if focused != nil && (visible[i] == focused || visible[i+1] == focused) {
		return dividerFocusStyle
	}
	return dividerDimStyle
}

// visibleRegions returns regions that are both user-visible and not collapsed by layout.
func (rm *RegionManager) visibleRegions() []*Region {
	var result []*Region
	for _, r := range rm.Regions {
		if r.Visible && !r.collapsed {
			result = append(result, r)
		}
	}
	return result
}

// recalculate distributes available space among visible regions.
func (rm *RegionManager) recalculate() {
	// Reset collapsed state — recalculated every time based on current size
	for _, r := range rm.Regions {
		r.collapsed = false
	}

	// Get user-visible regions (collapsed is now all false)
	visible := rm.visibleRegions()
	if len(visible) == 0 {
		return
	}

	if rm.Direction == Horizontal {
		rm.recalcHorizontal(visible)
	} else {
		rm.recalcVertical(visible)
	}

	// Clamp focus index to visible (post-collapse) regions
	actualVisible := rm.visibleRegions()
	if len(actualVisible) > 0 && rm.FocusIdx >= len(actualVisible) {
		rm.FocusIdx = len(actualVisible) - 1
	}
}

func (rm *RegionManager) recalcHorizontal(visible []*Region) {
	// Island layout: each pane has its own full border (2 cols), no gap.
	// Total overhead: 2*n. Single pane: no borders (handled in Render).
	n := len(visible)
	borderCols := 0
	borderRows := 0
	if n > 1 {
		borderCols = 2 * n // left + right border per pane
		borderRows = 2     // top + bottom border rows
	}

	available := rm.Width - borderCols
	paneHeight := rm.Height - borderRows

	// Check if all panes fit at minimum width
	if available < len(visible)*MinPaneWidth {
		// Not enough space — collapse all but the first pane
		visible[0].x = 0
		visible[0].y = 0
		visible[0].width = rm.Width
		visible[0].height = rm.Height
		visible[0].Pane.SetSize(rm.Width, rm.Height)
		for _, r := range visible[1:] {
			r.width = 0
			r.height = 0
			r.collapsed = true
		}
		return
	}

	// Allocate minimum width first, then distribute remaining space by ratio
	totalRatio := 0.0
	for _, r := range visible {
		totalRatio += r.Ratio
	}

	remaining := available - len(visible)*MinPaneWidth
	x := 0
	allocatedExtra := 0
	for i, r := range visible {
		extra := extraForIndex(i, len(visible), remaining, r.Ratio, totalRatio, &allocatedExtra)
		w := MinPaneWidth + extra

		// Content starts after left border (1 col) and top border (1 row).
		if n > 1 {
			r.x = x + 1 // +1 for left border char
			r.y = 1     // top border row
		} else {
			r.x = x
			r.y = 0
		}
		r.width = w
		r.height = paneHeight
		r.Pane.SetSize(w, paneHeight)

		x += w
		if n > 1 {
			x += 2 // left + right border chars
		}
	}
}

// extraForIndex distributes remaining space for pane at index i.
// Falls back to even distribution when totalRatio is zero.
func extraForIndex(i, n, remaining int, ratio, totalRatio float64, allocated *int) int {
	if remaining <= 0 {
		return 0
	}
	var extra int
	if i == n-1 {
		// Last pane gets whatever remains to avoid rounding gaps
		extra = remaining - *allocated
	} else if totalRatio > 0 {
		extra = int(float64(remaining) * (ratio / totalRatio))
	} else {
		extra = remaining / n
	}
	if extra < 0 {
		extra = 0
	}
	*allocated += extra
	return extra
}

const minPaneHeight = 3

func (rm *RegionManager) recalcVertical(visible []*Region) {
	dividers := len(visible) - 1
	available := rm.Height - dividers

	// Not enough space — collapse all but first
	if available < len(visible)*minPaneHeight {
		visible[0].x = 0
		visible[0].y = 0
		visible[0].width = rm.Width
		visible[0].height = rm.Height
		visible[0].Pane.SetSize(rm.Width, rm.Height)
		for _, r := range visible[1:] {
			r.width = 0
			r.height = 0
			r.collapsed = true
		}
		return
	}

	// Allocate minimum height first, then distribute remaining space by ratio
	totalRatio := 0.0
	for _, r := range visible {
		totalRatio += r.Ratio
	}

	remaining := available - len(visible)*minPaneHeight
	y := 0
	allocatedExtra := 0
	for i, r := range visible {
		extra := extraForIndex(i, len(visible), remaining, r.Ratio, totalRatio, &allocatedExtra)
		h := minPaneHeight + extra

		r.x = 0
		r.y = y
		r.width = rm.Width
		r.height = h
		r.Pane.SetSize(rm.Width, h)

		y += h
		if i < len(visible)-1 {
			y++ // divider
		}
	}
}
