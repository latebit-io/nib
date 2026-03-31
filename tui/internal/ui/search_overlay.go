package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/latebit-io/junto/engine/search"
	"github.com/mattn/go-runewidth"
)

// searchResultMsg delivers async search results back to the UI.
type searchResultMsg struct {
	results []search.Result
	err     error
}

// SearchOpenFileMsg is emitted when the user selects a search result.
// AppModel catches this to open the file and navigate to the line.
type SearchOpenFileMsg struct {
	Path string
	Line int
}

// SearchOverlayModel is a floating overlay for project-wide search.
type SearchOverlayModel struct {
	// Active indicates whether the search overlay is visible.
	Active bool
	// Query is the current search input text.
	Query string
	// Results holds the search matches from the last search.
	Results []search.Result
	// Selected is the index of the currently highlighted result.
	Selected int
	// ScrollOffset is the index of the first visible result.
	ScrollOffset int
	// Width is the terminal width (set during render).
	Width int
	// Height is the terminal height (set during render).
	Height int
	// ProjectRoot is the absolute path to the project root for search.
	ProjectRoot string
	// Searching indicates an async search is in progress.
	Searching bool
	// ErrorMsg holds the error from the last failed search.
	ErrorMsg string
}

// Search overlay constants.
const (
	searchMinWidth     = 60
	searchMaxVisible   = 20
	searchInputHeight  = 2
	searchFooterHeight = 1
)

// Search overlay styles.
var (
	searchBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("62"))
	searchInputStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("252"))
	searchCursorStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("252")).
				Foreground(lipgloss.Color("0"))
	searchSelectedStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("62")).
				Foreground(lipgloss.Color("230"))
	searchPathStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("245"))
	searchLineNumStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("166"))
	searchDimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240"))
)

// Open activates the search overlay.
func (s *SearchOverlayModel) Open() {
	s.Active = true
	s.Query = ""
	s.Results = nil
	s.Selected = 0
	s.ScrollOffset = 0
	s.Searching = false
	s.ErrorMsg = ""
}

// Close deactivates the search overlay.
func (s *SearchOverlayModel) Close() {
	s.Active = false
	s.Results = nil
}

// Update handles input for the search overlay.
func (s *SearchOverlayModel) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return s.handleKey(msg)
	case searchResultMsg:
		s.Searching = false
		if msg.err != nil {
			s.ErrorMsg = msg.err.Error()
			s.Results = nil
		} else {
			s.Results = msg.results
			s.ErrorMsg = ""
		}
		s.Selected = 0
		s.ScrollOffset = 0
		return nil
	}
	return nil
}

func (s *SearchOverlayModel) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case msg.Code == tea.KeyEscape:
		s.Close()
		return nil
	case msg.Code == tea.KeyEnter && msg.Mod == 0:
		if len(s.Results) > 0 && s.Selected < len(s.Results) {
			r := s.Results[s.Selected]
			s.Close()
			return func() tea.Msg {
				return SearchOpenFileMsg{Path: r.Path, Line: r.Line}
			}
		}
		// No results yet — trigger search.
		if s.Query != "" && !s.Searching {
			return s.runSearch()
		}
		return nil
	case msg.Code == tea.KeyUp:
		if s.Selected > 0 {
			s.Selected--
			s.ensureVisible()
		}
		return nil
	case msg.Code == tea.KeyDown:
		if s.Selected < len(s.Results)-1 {
			s.Selected++
			s.ensureVisible()
		}
		return nil
	case msg.Code == tea.KeyBackspace:
		if len(s.Query) > 0 {
			runes := []rune(s.Query)
			s.Query = string(runes[:len(runes)-1])
		}
		return nil
	default:
		if msg.Text != "" && msg.Mod == 0 {
			s.Query += msg.Text
		}
		return nil
	}
}

func (s *SearchOverlayModel) runSearch() tea.Cmd {
	s.Searching = true
	query := s.Query
	root := s.ProjectRoot
	return func() tea.Msg {
		results, err := search.Search(root, query, search.Options{
			MaxResults: 200,
		})
		return searchResultMsg{results: results, err: err}
	}
}

func (s *SearchOverlayModel) ensureVisible() {
	maxVis := s.maxVisible()
	if s.Selected < s.ScrollOffset {
		s.ScrollOffset = s.Selected
	}
	if s.Selected >= s.ScrollOffset+maxVis {
		s.ScrollOffset = s.Selected - maxVis + 1
	}
}

func (s *SearchOverlayModel) maxVisible() int {
	available := s.Height/2 - searchInputHeight - searchFooterHeight - 2 // border
	if available < 3 {
		available = 3
	}
	if available > searchMaxVisible {
		available = searchMaxVisible
	}
	return available
}

// RenderOverlay draws the search overlay on top of the background view.
func (s *SearchOverlayModel) RenderOverlay(background string, width, height int) string {
	s.Width = width
	s.Height = height

	boxWidth := width - 2
	if boxWidth < searchMinWidth {
		boxWidth = searchMinWidth
	}
	if boxWidth > width {
		boxWidth = width
	}
	innerWidth := boxWidth - 2
	if innerWidth < 1 {
		innerWidth = 1
	}

	// Input line with cursor.
	qRunes := []rune(s.Query)
	displayRunes := append(qRunes, ' ')
	cursorIdx := len(qRunes)
	maxInputWidth := max(1, innerWidth-3) // leave room for icon
	if len(displayRunes) > maxInputWidth {
		start := len(displayRunes) - maxInputWidth
		displayRunes = displayRunes[start:]
		cursorIdx = len(displayRunes) - 1
	}
	var inputLine strings.Builder
	inputLine.WriteString(searchDimStyle.Render(" > "))
	for i, r := range displayRunes {
		ch := string(r)
		if i == cursorIdx {
			inputLine.WriteString(searchCursorStyle.Render(ch))
		} else {
			inputLine.WriteString(searchInputStyle.Render(ch))
		}
	}
	inputRendered := inputLine.String()

	// Results list.
	maxVis := s.maxVisible()
	resultLines := make([]string, 0, maxVis)
	end := s.ScrollOffset + maxVis
	if end > len(s.Results) {
		end = len(s.Results)
	}

	for i := s.ScrollOffset; i < end; i++ {
		r := s.Results[i]
		isSelected := i == s.Selected
		line := s.renderResult(r, isSelected, innerWidth)
		resultLines = append(resultLines, line)
	}

	blankLine := strings.Repeat(" ", innerWidth)
	for len(resultLines) < maxVis {
		resultLines = append(resultLines, blankLine)
	}

	// Footer.
	var footer string
	if s.Searching {
		footer = searchDimStyle.Render(" searching...")
	} else if s.ErrorMsg != "" {
		footer = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render(" " + s.ErrorMsg)
	} else if len(s.Results) > 0 {
		footer = searchDimStyle.Render(fmt.Sprintf(" %d result(s) — Enter to open, Esc to close", len(s.Results)))
	} else if s.Query != "" {
		footer = searchDimStyle.Render(" No results — press Enter to search")
	} else {
		footer = searchDimStyle.Render(" Type a query, press Enter to search")
	}

	content := inputRendered + "\n" +
		" " + strings.Repeat("─", max(0, innerWidth-1)) + "\n" +
		strings.Join(resultLines, "\n") + "\n" +
		footer

	box := searchBorderStyle.Width(boxWidth).Render(content)

	// Overlay onto background.
	bgLines := strings.Split(background, "\n")
	for len(bgLines) < height {
		bgLines = append(bgLines, strings.Repeat(" ", width))
	}

	boxLines := strings.Split(box, "\n")
	topPad := height / 4
	if topPad < 1 {
		topPad = 1
	}
	leftPad := (width - boxWidth) / 2
	if leftPad < 0 {
		leftPad = 0
	}

	for i, line := range boxLines {
		row := topPad + i
		if row >= 0 && row < height && row < len(bgLines) {
			padding := strings.Repeat(" ", leftPad)
			bgLines[row] = padding + line
		}
	}

	return strings.Join(bgLines[:height], "\n")
}

func (s *SearchOverlayModel) renderResult(r search.Result, selected bool, maxWidth int) string {
	prefix := fmt.Sprintf(" %s:%d: ", r.Path, r.Line)
	prefixWidth := runewidth.StringWidth(prefix)

	textWidth := maxWidth - prefixWidth
	if textWidth < 10 {
		textWidth = 10
	}

	text := strings.TrimSpace(r.Text)
	if runewidth.StringWidth(text) > textWidth {
		text = runewidth.Truncate(text, textWidth-1, "…")
	}

	if selected {
		full := prefix + text
		if runewidth.StringWidth(full) < maxWidth {
			full += strings.Repeat(" ", maxWidth-runewidth.StringWidth(full))
		}
		return searchSelectedStyle.Render(full)
	}

	return searchPathStyle.Render(fmt.Sprintf(" %s", r.Path)) +
		searchLineNumStyle.Render(fmt.Sprintf(":%d: ", r.Line)) +
		searchInputStyle.Render(text)
}
