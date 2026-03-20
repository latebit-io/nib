package ui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
	Visible bool

	// Calculated by SetSize — position in global coordinates.
	x, y, width, height int
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

// RegionAt returns the region at global coordinate x and the
// local x offset within that region. Returns nil if no region is hit.
func (rm *RegionManager) RegionAt(x int) (*Region, int) {
	for _, r := range rm.visibleRegions() {
		if x >= r.x && x < r.x+r.width {
			return r, x - r.x
		}
	}
	return nil, 0
}

// HandleMouse translates global mouse coordinates to pane-local coordinates,
// sets focus on the clicked region, and forwards the event to the pane.
func (rm *RegionManager) HandleMouse(msg tea.MouseMsg) tea.Cmd {
	region, localX := rm.RegionAt(msg.X)
	if region == nil {
		return nil
	}

	// Set focus to clicked region
	rm.FocusByName(region.Name)

	// Forward mouse with translated X coordinate
	localMsg := msg
	localMsg.X = localX
	return region.Pane.Update(localMsg)
}

// Render composes all visible pane renders with dividers between them.
// Returns exactly rm.Height lines joined by newlines.
func (rm *RegionManager) Render() string {
	visible := rm.visibleRegions()
	if len(visible) == 0 || rm.Height == 0 {
		return ""
	}

	if len(visible) == 1 {
		return visible[0].Pane.Render()
	}

	dividerStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("240")).
		Background(lipgloss.Color("235"))
	div := dividerStyle.Render("│")

	// Render each pane and split into lines
	paneLines := make([][]string, len(visible))
	for i, r := range visible {
		paneLines[i] = strings.Split(r.Pane.Render(), "\n")
	}

	// Compose line by line
	output := make([]string, rm.Height)
	for row := range rm.Height {
		var line strings.Builder
		for i, pl := range paneLines {
			if i > 0 {
				line.WriteString(div)
			}
			if row < len(pl) {
				line.WriteString(pl[row])
			}
		}
		output[row] = line.String()
	}

	return strings.Join(output, "\n")
}

// visibleRegions returns only regions with Visible=true.
func (rm *RegionManager) visibleRegions() []*Region {
	var result []*Region
	for _, r := range rm.Regions {
		if r.Visible {
			result = append(result, r)
		}
	}
	return result
}

// recalculate distributes available space among visible regions.
func (rm *RegionManager) recalculate() {
	visible := rm.visibleRegions()
	if len(visible) == 0 {
		return
	}

	if rm.Direction == Horizontal {
		rm.recalcHorizontal(visible)
	} else {
		rm.recalcVertical(visible)
	}

	// Clamp focus index
	if rm.FocusIdx >= len(visible) {
		rm.FocusIdx = len(visible) - 1
	}
}

func (rm *RegionManager) recalcHorizontal(visible []*Region) {
	dividers := len(visible) - 1
	available := rm.Width - dividers

	// Check if all panes fit at minimum width
	if available < len(visible)*MinPaneWidth {
		// Not enough space — show only the first pane at full width
		visible[0].x = 0
		visible[0].width = rm.Width
		visible[0].height = rm.Height
		visible[0].Pane.SetSize(rm.Width, rm.Height)
		for _, r := range visible[1:] {
			r.width = 0
			r.height = 0
			r.Visible = false
		}
		return
	}

	// Normalize ratios among visible regions
	totalRatio := 0.0
	for _, r := range visible {
		totalRatio += r.Ratio
	}

	x := 0
	for i, r := range visible {
		w := int(float64(available) * (r.Ratio / totalRatio))
		if w < MinPaneWidth {
			w = MinPaneWidth
		}
		// Last region gets remaining space to avoid rounding gaps
		if i == len(visible)-1 {
			w = rm.Width - x - dividers + i
			if w < MinPaneWidth {
				w = MinPaneWidth
			}
		}
		r.x = x
		r.y = 0
		r.width = w
		r.height = rm.Height
		r.Pane.SetSize(w, rm.Height)

		x += w + 1 // +1 for divider
	}
}

func (rm *RegionManager) recalcVertical(visible []*Region) {
	dividers := len(visible) - 1
	available := rm.Height - dividers

	totalRatio := 0.0
	for _, r := range visible {
		totalRatio += r.Ratio
	}

	y := 0
	for i, r := range visible {
		h := int(float64(available) * (r.Ratio / totalRatio))
		if h < 3 {
			h = 3
		}
		if i == len(visible)-1 {
			h = rm.Height - y - dividers + i
			if h < 3 {
				h = 3
			}
		}
		r.x = 0
		r.y = y
		r.width = rm.Width
		r.height = h
		r.Pane.SetSize(rm.Width, h)

		y += h + 1
	}
}
