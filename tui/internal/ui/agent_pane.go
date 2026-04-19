package ui

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/tui/internal/sanitize"
	"github.com/latebit-io/junto/tui/internal/ui/textarea"
	"github.com/mattn/go-runewidth"
)

// usageState tracks cumulative token consumption for display.
// Per-turn, the best available value is used: provider-reported if non-zero,
// otherwise client-side estimate. This handles mixed runs correctly.
type usageState struct {
	totalIn           int  // best-available input tokens (provider or estimate per turn)
	totalOut          int  // best-available output tokens (provider or estimate per turn)
	totalCached       int  // provider-reported cached tokens (exact, 0 if unavailable)
	hasExact          bool // true if any turn reported provider data
	turns             int
	streamingChars    int // characters received via AppendToken since last turn completed
	streamingInputEst int // current LLM call's input estimate (set before Stream, cleared on turn end)
}

// SetStreamingInput updates the current LLM call's input estimate.
// Called right before Stream() starts so the status bar can show input cost
// in real-time while the response is streaming.
func (m *AgentPaneModel) SetStreamingInput(e event.AgentInputEstimate) {
	m.usage.streamingInputEst = e.System + e.Tools + e.History + e.New
}

// UpdateUsage accumulates token counts from a turn usage event.
// Uses provider-reported values when available, falls back to client-side
// estimates. Resets the streaming counters since the turn data supersedes them.
func (m *AgentPaneModel) UpdateUsage(u event.AgentTurnUsage) {
	turnHasProvider := u.PromptTokens > 0 || u.CompletionTokens > 0
	if turnHasProvider {
		m.usage.totalIn += u.PromptTokens
		m.usage.totalOut += u.CompletionTokens
		m.usage.totalCached += u.CachedTokens
		m.usage.hasExact = true
	} else {
		m.usage.totalIn += u.SystemEst + u.ToolsEst + u.HistoryEst + u.NewEst
		m.usage.totalOut += u.CompletionEst
	}
	m.usage.streamingChars = 0
	m.usage.streamingInputEst = 0
	m.usage.turns++
}

// UsageIndicator returns a compact string for the main editor status bar.
// Updates in real-time: shows streaming input/output estimates while tokens
// arrive. Uses ~ prefix when only estimates are available.
func (m *AgentPaneModel) UsageIndicator() string {
	streamOut := (m.usage.streamingChars + 3) / 4
	streamIn := m.usage.streamingInputEst

	in := m.usage.totalIn + streamIn
	out := m.usage.totalOut + streamOut

	if in == 0 && out == 0 {
		return ""
	}

	prefix := "~"
	if m.usage.hasExact {
		prefix = ""
	}
	s := prefix + formatTokenCount(in) + "↓"
	if m.usage.totalCached > 0 && m.usage.totalIn > 0 {
		pct := m.usage.totalCached * 100 / m.usage.totalIn
		s += fmt.Sprintf("(%d%%⚡)", pct)
	}
	s += " " + prefix + formatTokenCount(out) + "↑"
	return s
}

// ResetUsage clears accumulated usage for a new agent run.
func (m *AgentPaneModel) ResetUsage() {
	m.usage = usageState{}
}

// formatTokenCount renders a token count as a compact string.
// < 1000 → "847", ≥ 1000 → "12.3k", ≥ 999950 → "1.0M".
func formatTokenCount(n int) string {
	switch {
	case n >= 999_950: // %.1f rounds 999950+ to 1000.0k — use M instead
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// formatTurnUsage produces a dim metadata line for a single turn.
// Uses provider data when available, otherwise falls back to estimates.
func formatTurnUsage(u event.AgentTurnUsage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n[turn %d", u.Turn)

	hasProvider := u.PromptTokens > 0 || u.CompletionTokens > 0
	if hasProvider {
		fmt.Fprintf(&b, ": %s in", formatTokenCount(u.PromptTokens))
		if u.CachedTokens > 0 {
			fmt.Fprintf(&b, " (%s cached)", formatTokenCount(u.CachedTokens))
		}
		fmt.Fprintf(&b, " · %s out", formatTokenCount(u.CompletionTokens))
	}
	if u.ToolCalls > 0 {
		fmt.Fprintf(&b, " · %d tools", u.ToolCalls)
	}

	// Composition estimate — always available.
	total := u.SystemEst + u.ToolsEst + u.HistoryEst + u.NewEst
	if total > 0 {
		if !hasProvider {
			fmt.Fprintf(&b, ": ~%s in · ~%s out", formatTokenCount(total), formatTokenCount(u.CompletionEst))
		}
		fmt.Fprintf(&b, " | sys:%s tools:%s hist:%s new:%s",
			formatTokenCount(u.SystemEst),
			formatTokenCount(u.ToolsEst),
			formatTokenCount(u.HistoryEst),
			formatTokenCount(u.NewEst))
	}
	b.WriteString("]\n")
	return b.String()
}

// formatCompacted produces a dim metadata line when conversation history
// is compacted. Shows tokens before and after so the developer can see
// how much was saved.
func formatCompacted(e event.AgentCompacted) string {
	saved := e.BeforeTokens - e.AfterTokens
	return fmt.Sprintf("\n[compacted: %s → %s history (saved %s)]\n",
		formatTokenCount(e.BeforeTokens),
		formatTokenCount(e.AfterTokens),
		formatTokenCount(saved))
}

// formatSessionSummary produces the summary shown when the agent finishes.
func formatSessionSummary(u usageState) string {
	if u.turns == 0 {
		return ""
	}
	prefix := "~"
	if u.hasExact {
		prefix = ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Session: %d turns", u.turns)
	if u.totalIn > 0 || u.totalOut > 0 {
		fmt.Fprintf(&b, " · %s%s in", prefix, formatTokenCount(u.totalIn))
		if u.totalCached > 0 && u.totalIn > 0 {
			pct := u.totalCached * 100 / u.totalIn
			fmt.Fprintf(&b, " (%s cached, %d%%)", formatTokenCount(u.totalCached), pct)
		}
		fmt.Fprintf(&b, " · %s%s out", prefix, formatTokenCount(u.totalOut))
	}
	return b.String()
}

// inputHeight returns the number of rows reserved for the input area
// (separator + input + status). Uses 1/6 of the pane height, minimum 5.
func (m *AgentPaneModel) inputHeight() int {
	h := m.height / 6
	if h < 5 {
		h = 5
	}
	return h
}

// modelSelHeight returns the number of rows reserved for the bottom area
// when the model selector is active. Delegates to ModelSelectorModel.Height.
func (m *AgentPaneModel) modelSelHeight() int {
	return m.ModelSel.Height(m.height)
}

// inlineDisplayReplacer strips newlines/carriage returns that would break
// single-line display labels in the selector and status line.
var inlineDisplayReplacer = strings.NewReplacer("\r", " ", "\n", " ")

// sanitizeInlineDisplay strips ANSI escapes and forces single-line output.
func sanitizeInlineDisplay(s string) string {
	var san sanitize.Sanitizer
	return inlineDisplayReplacer.Replace(san.Sanitize(s))
}

// fenceState records the active code fence after processing a raw line.
// Zero value means "not inside a code block".
type fenceState struct {
	char rune // '`' or '~', 0 when outside a block
	len  int  // fence run length, 0 when outside a block
}

// Package-level styles — allocated once, never in render paths.
var (
	userMessageStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("230")).Bold(true)
	agentDimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	agentStatusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)
	agentAwaitStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("13")).Bold(true)
	agentSelStyle    = lipgloss.NewStyle().Background(lipgloss.Color("24"))
	agentInputStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("230"))
	agentInputDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	agentCursorStyle = lipgloss.NewStyle().Reverse(true)
)

// awaitingInputState tracks a pending request_input prompt from the agent.
// Non-nil means the agent is blocked on the developer's typed answer.
// Typing + Enter sends the answer; Esc cancels the agent run.
type awaitingInputState struct {
	// Prompt is the question shown to the developer (for reference; the
	// text has already been appended to the transcript by ShowAwaitingInput).
	Prompt string
	// Options were the suggested choices (for reference).
	Options []event.AwaitingInputOption
	// CallID correlates the answer back to the originating tool call.
	CallID string
}

// AgentPaneModel is the Bubble Tea model for the agent reasoning pane.
type AgentPaneModel struct {
	// Layout — set via SetSize, read by Render and mouse hit-testing.
	width  int
	height int

	// RawLines stores unwrapped content lines before word-wrapping.
	RawLines []string
	// Lines holds the word-wrapped display lines derived from RawLines.
	Lines        []string
	wrappedIndex []int

	// userRawLines tracks which raw line indices are user messages.
	// Stable across rewrap — translated to wrapped indices via wrappedIndex in Render().
	userRawLines map[int]bool

	// ScrollOffset is the first visible line index in the output area.
	ScrollOffset int

	// status is the current agent status (idle, thinking, reviewing, etc.).
	// Set via SetStatus, read via StatusKind.
	status event.StatusKind

	// modelLabel is the display name of the active LLM model (e.g. "gemini-2.5-flash").
	// Shown on the left side of the status line. Set via SetModelLabel.
	modelLabel string

	// usage tracks cumulative token consumption for the status line display.
	usage usageState

	// ModelSel holds the inline model selector state — when active,
	// replaces the input area with a model list.
	ModelSel                ModelSelectorModel
	modelSelPrevInputActive bool // input focus state before selector opened

	// Output selection — fully internal, driven by mouse events on the
	// output region. Not accessed outside agent_pane.go.
	selActive   bool
	selDragging bool
	selStartLn  int
	selStartCol int
	cursorLn    int
	cursorCol   int

	// Input area
	inputActive       bool               // true when the textarea has keyboard focus
	input             *textarea.TextArea // multiline text editor for user input
	planningMode      bool               // true when input will start a planning conversation
	inputAreaStartRow int                // first row of the input area (mouse hit-testing)
	inputAreaEndRow   int                // exclusive end row
	inputScrollOffset int                // first visible visual row in the textarea
	inputDragging     bool               // true while dragging inside the input area
	inputPadLeft      int                // cell offset from pane left edge to textarea content

	// API key input mode
	apiKeyInputActive bool   // true when collecting an API key from the user
	apiKeyProfile     string // profile the key is for
	apiKeyBuffer      string // accumulated key text (masked in display)

	// Shared services
	services *Services

	// Stateful sanitizer for streamed text
	sanitizer sanitize.Sanitizer

	// inCodeAfter[i] is true when Lines[i] is inside a code block after
	// processing that line (i.e. the opening fence flips it to true, the
	// closing fence flips it back to false). Used by isCodeLine().
	inCodeAfter []bool

	// rawFenceAfter[i] records the active fence state after processing
	// RawLines[i]. This allows incremental recomputation in O(new_lines)
	// instead of rescanning the entire prefix on every token append.
	rawFenceAfter []fenceState

	// mdCache holds rendered markdown for Lines in the visible viewport only.
	// mdCacheOffset is the ScrollOffset the cache was built for; when the viewport
	// moves or content changes, the cache is invalidated. Bounded to O(VisibleLines).
	mdCache       []string
	mdCacheOffset int

	// hasAgent is true when an LLM provider is configured.
	hasAgent bool

	// awaitingInput is non-nil when the agent is blocked on a request_input
	// prompt. Controls status-bar rendering and rewires Enter/Esc so the
	// textarea submits an answer (or cancels the run) instead of a goal.
	awaitingInput *awaitingInputState
}

// NewAgentPaneModel creates a new agent pane.
func NewAgentPaneModel(svc *Services, hasAgent bool) *AgentPaneModel {
	input := textarea.New(1) // width set properly in SetSize
	input.SetClipboard(svc.Clipboard)
	return &AgentPaneModel{
		status:   event.StatusIdle,
		services: svc,
		input:    input,
		hasAgent: hasAgent,
	}
}

// --- Accessors ---

// SetStatus updates the agent status displayed in the status bar.
func (m *AgentPaneModel) SetStatus(s event.StatusKind) { m.status = s }

// ShowAwaitingInput registers a pending request_input prompt. Renders a
// styled prompt block into the transcript (so it scrolls with content and
// survives rewrap on resize), focuses the textarea, and flips the pane
// into awaiting mode so Enter submits an answer instead of a goal. The
// textarea is reset so any lingering draft from a prior mode cannot be
// accidentally submitted as the answer.
func (m *AgentPaneModel) ShowAwaitingInput(e event.AgentAwaitingInput) {
	m.awaitingInput = &awaitingInputState{
		Prompt:  e.Prompt,
		Options: e.Options,
		CallID:  e.CallID,
	}
	m.AppendMeta(renderAwaitingInputBlock(e))
	m.input.Reset()
	m.inputActive = true
	m.recomputeInputLayout()
}

// ClearAwaitingInput drops any pending request_input state without
// appending transcript output. Called when the agent run ends (AgentDone,
// AgentError) so a cancel or failure leaves the pane in a clean state.
// Also drops any draft answer in the textarea — it belonged to the
// abandoned prompt and must not leak into the next goal submission.
func (m *AgentPaneModel) ClearAwaitingInput() {
	if m.awaitingInput == nil {
		return
	}
	m.awaitingInput = nil
	m.input.Reset()
	m.recomputeInputLayout()
}

// IsAwaitingInput reports whether the agent is blocked on a structured
// input answer. Used by tests and by callers that want to gate UI based
// on the agent's mid-turn state.
func (m *AgentPaneModel) IsAwaitingInput() bool { return m.awaitingInput != nil }

// renderAwaitingInputBlock formats the prompt, reason, and options into a
// styled transcript block. Kept as a pure function so the test suite can
// assert on the rendered output without a live pane model.
func renderAwaitingInputBlock(e event.AgentAwaitingInput) string {
	var b strings.Builder
	b.WriteString("\n┌─ Agent needs your input ─\n")
	b.WriteString("│ ")
	b.WriteString(e.Prompt)
	b.WriteString("\n")
	if strings.TrimSpace(e.Reason) != "" {
		b.WriteString("│ ")
		b.WriteString(e.Reason)
		b.WriteString("\n")
	}
	if len(e.Options) > 0 {
		b.WriteString("│\n")
		for _, opt := range e.Options {
			b.WriteString("│   ")
			b.WriteString(opt.ID)
			b.WriteString(" — ")
			b.WriteString(opt.Label)
			b.WriteString("\n")
		}
	}
	b.WriteString("│\n")
	b.WriteString("│ Type your answer and press Enter. Esc cancels the run.\n")
	b.WriteString("└─\n")
	return b.String()
}

// StatusKind returns the current agent status.
func (m *AgentPaneModel) StatusKind() event.StatusKind { return m.status }

// SetModelLabel sets the display name shown in the agent pane status line.
func (m *AgentPaneModel) SetModelLabel(label string) { m.modelLabel = label }

// SetHasAgent updates the pane's cached agent-present flag. Called when the
// agent is constructed mid-session (e.g. first OAuth connect) so the pane
// leaves "No LLM configured" mode without a restart.
func (m *AgentPaneModel) SetHasAgent(has bool) { m.hasAgent = has }

// OpenModelSelector activates the inline model selector, replacing the input area.
// profiles is the full list of available profiles (for Tab cycling); may be nil.
func (m *AgentPaneModel) OpenModelSelector(items []ModelSelectorItem, profile, currentModel string, profiles []string) {
	m.modelSelPrevInputActive = m.inputActive
	m.ModelSel.Open(items, profile, currentModel, profiles)
	m.inputActive = false // selector replaces input area — deactivate textarea
	m.recomputeInputLayout()
}

// CloseModelSelector deactivates the inline model selector and restores
// the input focus state that was active before the selector opened.
func (m *AgentPaneModel) CloseModelSelector() {
	m.ModelSel.Close()
	m.inputActive = m.modelSelPrevInputActive
	m.recomputeInputLayout()
	m.clampScroll()
}

// IsModelSelectorActive reports whether the inline model selector is open.
func (m *AgentPaneModel) IsModelSelectorActive() bool { return m.ModelSel.IsActive() }

// StartAPIKeyInput activates the API key input mode for a profile.
// Shows a masked input field where the user can type their API key.
func (m *AgentPaneModel) StartAPIKeyInput(profile string) {
	m.apiKeyInputActive = true
	m.apiKeyProfile = profile
	m.apiKeyBuffer = ""
	m.inputActive = false
}

// IsAPIKeyInputActive reports whether the API key input mode is active.
func (m *AgentPaneModel) IsAPIKeyInputActive() bool { return m.apiKeyInputActive }

// handleAPIKeyInput processes key events during API key input.
func (m *AgentPaneModel) handleAPIKeyInput(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.Code {
	case tea.KeyEscape:
		m.apiKeyInputActive = false
		m.apiKeyBuffer = ""
		m.inputActive = true
		return nil
	case tea.KeyEnter:
		profile := m.apiKeyProfile
		key := m.apiKeyBuffer
		m.apiKeyInputActive = false
		m.apiKeyBuffer = ""
		m.apiKeyProfile = ""
		m.inputActive = true
		return func() tea.Msg {
			return apiKeyEnteredMsg{profile: profile, key: key}
		}
	case tea.KeyBackspace:
		if len(m.apiKeyBuffer) > 0 {
			m.apiKeyBuffer = m.apiKeyBuffer[:len(m.apiKeyBuffer)-1]
		}
		return nil
	default:
		if msg.Text != "" {
			m.apiKeyBuffer += msg.Text
		}
		return nil
	}
}

// UpdateModelSelector handles key input for the inline model selector.
// Returns a tea.Cmd if a selection or cancellation occurred.
func (m *AgentPaneModel) UpdateModelSelector(msg tea.KeyPressMsg) tea.Cmd {
	cmd := m.ModelSel.Update(msg)
	if !m.ModelSel.IsActive() {
		// Selector was closed by the update — restore input focus and recompute layout.
		m.inputActive = m.modelSelPrevInputActive
		m.recomputeInputLayout()
		m.clampScroll()
	}
	return cmd
}

// SetInputActive sets keyboard focus on or off for the input textarea.
func (m *AgentPaneModel) SetInputActive(active bool) {
	m.inputActive = active
	m.recomputeInputLayout()
}

// IsInputActive reports whether the input textarea has keyboard focus.
func (m *AgentPaneModel) IsInputActive() bool { return m.inputActive }

// SetPlanningMode configures whether the next submission starts a planning
// conversation rather than a normal agent conversation.
func (m *AgentPaneModel) SetPlanningMode(planning bool) { m.planningMode = planning }

// ResetInput clears the textarea content and recomputes the input layout.
func (m *AgentPaneModel) ResetInput() {
	m.input.Reset()
	m.recomputeInputLayout()
}

// Clipboard returns the shared clipboard service.
func (m *AgentPaneModel) Clipboard() ClipboardService { return m.services.Clipboard }

// Title returns the pane title for display in the border. Implements Titled.
func (m *AgentPaneModel) Title() string {
	return "Junto"
}

// SetSize updates the agent pane dimensions and clamps scroll. Implements Pane.
// Re-wraps content when width changes so text reflows correctly.
func (m *AgentPaneModel) SetSize(width, height int) {
	oldWidth := m.width
	m.width = width
	m.height = height
	if width != oldWidth {
		m.rewrap()
	}
	m.input.SetSize(width)
	m.clampScroll()
	m.recomputeInputLayout()
}

// recomputeInputLayout updates the input area geometry and scroll offset.
// Must be called after any change to Height, input content, or cursor position
// so that mouse hit-testing uses current values.
func (m *AgentPaneModel) recomputeInputLayout() {
	// Input area sits between the separator line and the status line.
	bottomH := m.inputHeight()
	if m.ModelSel.IsActive() {
		bottomH = m.modelSelHeight()
	}
	contentEnd := m.height - bottomH
	if contentEnd < 0 {
		contentEnd = 0
	}
	m.inputAreaStartRow = contentEnd + 1 // after separator
	inputRows := m.height - m.inputAreaStartRow - 1
	if inputRows < 0 {
		inputRows = 0
	}
	m.inputAreaEndRow = m.inputAreaStartRow + inputRows

	// Scroll the textarea so the cursor row is visible.
	m.inputScrollOffset = 0
	if inputRows > 0 {
		cursorVisRow, _ := m.input.CursorPosition()
		lineCount := m.input.VisualLineCount()
		if lineCount > inputRows && cursorVisRow >= inputRows {
			m.inputScrollOffset = cursorVisRow - inputRows + 1
		}
		maxScroll := lineCount - inputRows
		if maxScroll < 0 {
			maxScroll = 0
		}
		if m.inputScrollOffset > maxScroll {
			m.inputScrollOffset = maxScroll
		}
	}
}

// Update handles messages for the agent pane. Implements Pane.
// Agent events (token, status, edit, error, done) are handled by AppModel
// and delivered via direct method calls (AppendToken, AppendText, etc.).
func (m *AgentPaneModel) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.MouseClickMsg:
		return m.handleMouseClick(msg)
	case tea.MouseMotionMsg:
		return m.handleMouseMotion(msg)
	case tea.MouseReleaseMsg:
		return m.handleMouseRelease(msg)
	case tea.MouseWheelMsg:
		return m.handleMouseWheel(msg)
	case tea.KeyPressMsg:
		if m.apiKeyInputActive {
			return m.handleAPIKeyInput(msg)
		}
		if m.inputActive {
			return m.handleInput(msg)
		}
		return m.handleKey(msg)
	}
	return nil
}

// AppendToken sanitizes and appends streaming text from the agent.
// Uses the stateful sanitizer to handle escape sequences split across chunks.
// Counts sanitized bytes for real-time output token estimation.
func (m *AgentPaneModel) AppendToken(text string) {
	clean := m.sanitizer.Sanitize(text)
	m.usage.streamingChars += len(clean)
	m.AppendText(clean)
}

// AppendMeta sanitizes and appends non-stream text (edit proposals, errors, status).
// Uses a one-shot sanitizer so it doesn't interfere with the streaming sanitizer state.
func (m *AgentPaneModel) AppendMeta(text string) {
	var s sanitize.Sanitizer
	m.AppendText(s.Sanitize(text))
}

// AppendUserMessage appends the developer's follow-up message as plain text
// and marks the raw lines so Render() can style them distinctly. Tracks raw
// line indices (not wrapped) so styling survives rewrap on resize.
func (m *AgentPaneModel) AppendUserMessage(text string) {
	var s sanitize.Sanitizer
	text = s.Sanitize(text)

	firstRaw := len(m.RawLines)
	m.AppendText("\n\nYou: " + text + "\n\n")
	// Exclude the trailing empty raw line — AppendText reuses the last
	// raw line for the first chunk of the next append, so marking it
	// would misclassify the first agent token as a user message.
	if m.userRawLines == nil {
		m.userRawLines = make(map[int]bool)
	}
	endRaw := len(m.RawLines)
	if endRaw > firstRaw && m.RawLines[endRaw-1] == "" {
		endRaw--
	}
	for i := firstRaw; i < endRaw; i++ {
		m.userRawLines[i] = true
	}

	// AppendText ran recomputeCodeBlock before user lines were marked, so
	// fence state may have advanced through user content (e.g. an unmatched
	// "```" in the message). Recompute from firstRaw now that userRawLines
	// is populated — this skips user lines and resets fence state correctly.
	m.recomputeCodeBlock(firstRaw)
	m.invalidateMdCache()
}

// recomputeCodeBlock rebuilds inCodeAfter starting from raw line index fromRaw.
// Fence detection runs on RawLines (logical lines) so that wrapping can never
// split or fabricate a fence. The per-raw-line state is then projected to all
// wrapped lines belonging to that raw line via wrappedIndex.
//
// Fence state is persisted in rawFenceAfter so incremental appends seed from
// rawFenceAfter[fromRaw-1] in O(1) instead of rescanning the entire prefix.
//
// Fences follow CommonMark rules: 3+ backticks or tildes, 0–3 leading spaces,
// opener can have info string, closer must use the same char at >= opener
// length with no non-space content after. This correctly handles nested fences
// (e.g. ```“ wrapping an inner ```).
func (m *AgentPaneModel) recomputeCodeBlock(fromRaw int) {
	// Size inCodeAfter to match Lines.
	for len(m.inCodeAfter) < len(m.Lines) {
		m.inCodeAfter = append(m.inCodeAfter, false)
	}
	m.inCodeAfter = m.inCodeAfter[:len(m.Lines)]

	// Truncate rawFenceAfter to fromRaw so we rebuild from there.
	if fromRaw < len(m.rawFenceAfter) {
		m.rawFenceAfter = m.rawFenceAfter[:fromRaw]
	}

	// Seed from persisted state — O(1).
	var fence fenceState
	if fromRaw > 0 && fromRaw-1 < len(m.rawFenceAfter) {
		fence = m.rawFenceAfter[fromRaw-1]
	}

	for ri := fromRaw; ri < len(m.RawLines); ri++ {
		// User messages are rendered with userMessageStyle, not markdown.
		// Skip them so an unmatched fence in user input doesn't bleed into
		// subsequent agent output.
		if m.userRawLines[ri] {
			m.rawFenceAfter = append(m.rawFenceAfter, fence)
			wStart := m.wrappedIndex[ri]
			wEnd := len(m.Lines)
			if ri+1 < len(m.wrappedIndex) {
				wEnd = m.wrappedIndex[ri+1]
			}
			for wi := wStart; wi < wEnd; wi++ {
				m.inCodeAfter[wi] = false
			}
			continue
		}

		fenceBefore := fence
		fc, fl, closeable := parseFenceLine(m.RawLines[ri])
		if fl > 0 {
			if fence.len == 0 {
				// Not in a code block — any fence opens one (info string allowed).
				fence = fenceState{char: fc, len: fl}
			} else if closeable && fc == fence.char && fl >= fence.len {
				// In a code block — only close if no trailing non-space content.
				fence = fenceState{}
			}
		}

		// A raw line is "code" if we were inside a fence before processing it
		// (body + closer) OR if processing it opened a fence (opener). This
		// ensures all wrapped segments of a closer line are marked as code,
		// not just the first one.
		lineIsCode := fenceBefore.len > 0 || fence.len > 0

		// Persist fence state for this raw line.
		m.rawFenceAfter = append(m.rawFenceAfter, fence)

		// Fill all wrapped lines that belong to this raw line.
		wStart := m.wrappedIndex[ri]
		wEnd := len(m.Lines)
		if ri+1 < len(m.wrappedIndex) {
			wEnd = m.wrappedIndex[ri+1]
		}
		for wi := wStart; wi < wEnd; wi++ {
			m.inCodeAfter[wi] = lineIsCode
		}
	}
}

// parseFenceLine checks if line is a code fence (opener or closer).
// Returns the fence character ('`' or '~'), the run length, and whether the
// line can act as a closer (no non-space content after the fence run).
// Returns 0, 0, false if the line is not a fence at all.
//
// CommonMark rules: 0–3 leading spaces, 3+ of the same fence char. An opener
// may have trailing info text (canClose=false). A closer must have only
// optional trailing spaces (canClose=true).
func parseFenceLine(line string) (ch rune, count int, canClose bool) {
	runes := []rune(line)
	i := 0

	// Skip 0–3 leading spaces.
	spaces := 0
	for i < len(runes) && runes[i] == ' ' && spaces < 3 {
		i++
		spaces++
	}
	if i >= len(runes) {
		return 0, 0, false
	}

	ch = runes[i]
	if ch != '`' && ch != '~' {
		return 0, 0, false
	}

	// Count consecutive fence chars.
	start := i
	for i < len(runes) && runes[i] == ch {
		i++
	}
	count = i - start
	if count < 3 {
		return 0, 0, false
	}

	// A closer requires only optional trailing spaces after the fence run.
	canClose = true
	for j := i; j < len(runes); j++ {
		if runes[j] != ' ' && runes[j] != '\t' {
			canClose = false
			break
		}
	}

	return ch, count, canClose
}

// isCodeLine returns true when Lines[i] should be rendered with code block styling.
// This covers the opening fence, body lines, and the closing fence — all wrapped
// segments of a raw line share the same flag.
func (m *AgentPaneModel) isCodeLine(i int) bool {
	if i < 0 || i >= len(m.inCodeAfter) {
		return false
	}
	return m.inCodeAfter[i]
}

func (m *AgentPaneModel) handleMouseWheel(msg tea.MouseWheelMsg) tea.Cmd {
	scrollLines := 3
	switch msg.Button {
	case tea.MouseWheelUp:
		m.ScrollOffset -= scrollLines
		if m.ScrollOffset < 0 {
			m.ScrollOffset = 0
		}
	case tea.MouseWheelDown:
		maxScroll := len(m.Lines) - m.VisibleLines()
		if maxScroll < 0 {
			maxScroll = 0
		}
		m.ScrollOffset += scrollLines
		if m.ScrollOffset > maxScroll {
			m.ScrollOffset = maxScroll
		}
	}
	return nil
}

func (m *AgentPaneModel) handleMouseClick(msg tea.MouseClickMsg) tea.Cmd {
	if !m.hasAgent {
		return nil
	}
	if msg.Button != tea.MouseLeft {
		m.inputDragging = false
		m.selDragging = false
		return nil
	}

	// Click in the input area → focus it and position cursor.
	if msg.Y >= m.inputAreaStartRow && msg.Y < m.inputAreaEndRow {
		m.inputActive = true
		m.inputDragging = true
		visRow := (msg.Y - m.inputAreaStartRow) + m.inputScrollOffset
		m.input.HandleClick(visRow, msg.X-m.inputPadLeft)
		return nil
	}
	m.inputActive = false
	m.inputDragging = false

	if msg.Y < 0 || msg.Y >= m.VisibleLines() {
		return nil
	}
	line, col := m.mouseToLineCol(msg.X, msg.Y)
	m.selActive = true
	m.selDragging = true
	m.selStartLn = line
	m.selStartCol = col
	m.cursorLn = line
	m.cursorCol = col
	return nil
}

func (m *AgentPaneModel) handleMouseMotion(msg tea.MouseMotionMsg) tea.Cmd {
	// Drag inside the input area → extend textarea selection.
	if m.inputDragging {
		visRow := (msg.Y - m.inputAreaStartRow) + m.inputScrollOffset
		m.input.HandleDrag(visRow, msg.X-m.inputPadLeft)
		return nil
	}

	if !m.selDragging || msg.Y < 0 || msg.Y >= m.VisibleLines() {
		return nil
	}
	line, col := m.mouseToLineCol(msg.X, msg.Y)
	m.cursorLn = line
	m.cursorCol = col
	return nil
}

func (m *AgentPaneModel) handleMouseRelease(_ tea.MouseReleaseMsg) tea.Cmd {
	m.inputDragging = false
	m.selDragging = false
	if m.cursorLn == m.selStartLn &&
		m.cursorCol == m.selStartCol {
		m.selActive = false
	}
	return nil
}

// mouseToLineCol converts mouse coordinates to line and column indices.
func (m *AgentPaneModel) mouseToLineCol(x, y int) (int, int) {
	line := m.ScrollOffset + y
	if line < 0 {
		line = 0
	}
	if line >= len(m.Lines) {
		line = max(len(m.Lines)-1, 0)
	}

	col := 0
	if line < len(m.Lines) {
		cellX := x
		if cellX < 0 {
			cellX = 0
		}
		runes := []rune(m.Lines[line])
		cellsSeen := 0
		col = len(runes) // default: past end of line
		for ri, r := range runes {
			w := runewidth.RuneWidth(r)
			if cellsSeen+w > cellX {
				col = ri
				break
			}
			cellsSeen += w
		}
	}
	return line, col
}

// handleInput delegates key handling to the TextArea component.
// Submit (Enter) and Cancel (Escape) are intercepted to manage
// the agent pane's input lifecycle.
func (m *AgentPaneModel) handleInput(msg tea.KeyPressMsg) tea.Cmd {
	cmd := m.input.Update(msg)
	m.recomputeInputLayout() // cursor/content may have changed
	if cmd == nil {
		return nil
	}

	// Inspect the command's message to handle submit/cancel.
	result := cmd()
	switch result.(type) {
	case textarea.SubmitMsg:
		text := m.input.Content()
		if strings.TrimSpace(text) == "" {
			if m.awaitingInput != nil {
				// Empty Enter while a prompt is pending: keep the
				// prompt visible, keep focus, let the developer keep
				// typing. Deactivating here would strand them with a
				// visible prompt and no way to answer without clicking.
				return nil
			}
			// Empty Enter outside awaiting: existing behavior — drop
			// focus and reset planning bit so the next click doesn't
			// silently submit as a plan.
			m.inputActive = false
			m.planningMode = false
			m.input.Reset()
			return nil
		}
		planning := m.planningMode
		awaiting := m.awaitingInput != nil
		m.inputActive = false
		m.input.Reset()
		m.planningMode = false
		if awaiting {
			// Answer the pending request_input prompt. Clearing
			// awaitingInput optimistically keeps state in sync with
			// what the agent will do next; a subsequent prompt will
			// re-establish it via ShowAwaitingInput.
			m.awaitingInput = nil
			return func() tea.Msg { return InputAnsweredMsg{Text: text} }
		}
		if planning {
			return func() tea.Msg { return PlanningGoalSubmittedMsg{Goal: text} }
		}
		return func() tea.Msg { return GoalSubmittedMsg{Goal: text} }
	case textarea.CancelMsg:
		// Esc while awaiting an input answer cancels the whole agent
		// run (matches Esc-rejects-edit semantics). Any other Esc just
		// deactivates focus and preserves content. The draft answer is
		// dropped — it belonged to the prompt we're abandoning, and
		// must not leak into the next goal submission.
		if m.awaitingInput != nil {
			m.awaitingInput = nil
			m.input.Reset()
			m.inputActive = false
			m.planningMode = false
			return func() tea.Msg { return CancelAgentMsg{} }
		}
		// Deactivate focus but preserve content — Ctrl+G or click restores it.
		// Clear planningMode so refocus via click doesn't silently submit as a plan.
		m.inputActive = false
		m.planningMode = false
		return nil
	default:
		// Re-wrap the command — we already consumed the thunk.
		return func() tea.Msg { return result }
	}
}

func (m *AgentPaneModel) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.Code {
	case tea.KeyUp:
		if m.ScrollOffset > 0 {
			m.ScrollOffset--
		}
	case tea.KeyDown:
		if m.ScrollOffset < len(m.Lines)-m.VisibleLines() {
			m.ScrollOffset++
		}
	case 'c':
		if msg.Mod == tea.ModCtrl {
			if m.selActive {
				if err := m.services.Clipboard.Write(m.SelectedText()); err != nil {
					slog.Warn("system clipboard write failed", "err", err)
				}
				m.selActive = false
			} else {
				if err := m.services.Clipboard.Write(strings.Join(m.Lines, "\n")); err != nil {
					slog.Warn("system clipboard write failed", "err", err)
				}
			}
		}
	}
	return nil
}

// AppendText adds streaming text to the agent pane.
// Raw text is stored unwrapped; wrapped Lines are derived incrementally
// for only the affected lines to avoid O(total_text) per token during streaming.
func (m *AgentPaneModel) AppendText(text string) {
	slog.Debug("agent pane append", "text_len", len(text))

	wasAtBottom := m.isAtBottom()

	parts := strings.Split(text, "\n")

	// Track which raw line index was modified vs appended
	firstAffected := len(m.RawLines) - 1
	if firstAffected < 0 {
		firstAffected = 0
	}

	for i, part := range parts {
		if i == 0 && len(m.RawLines) > 0 {
			m.RawLines[len(m.RawLines)-1] += part
		} else {
			m.RawLines = append(m.RawLines, part)
		}
	}

	// Incremental rewrap: truncate Lines to before firstAffected (O(1) via index),
	// then wrap only the affected raw lines.
	truncateTo := 0
	if firstAffected < len(m.wrappedIndex) {
		truncateTo = m.wrappedIndex[firstAffected]
	}
	m.Lines = m.Lines[:truncateTo]
	m.wrappedIndex = m.wrappedIndex[:firstAffected]
	if truncateTo < len(m.inCodeAfter) {
		m.inCodeAfter = m.inCodeAfter[:truncateTo]
	}
	m.invalidateMdCache()

	for i := firstAffected; i < len(m.RawLines); i++ {
		m.wrappedIndex = append(m.wrappedIndex, len(m.Lines))
		if m.width > 0 && runewidth.StringWidth(m.RawLines[i]) > m.width {
			m.Lines = append(m.Lines, m.wrapLine(m.RawLines[i])...)
		} else {
			m.Lines = append(m.Lines, m.RawLines[i])
		}
	}
	m.recomputeCodeBlock(firstAffected)

	if wasAtBottom {
		m.scrollToBottom()
	}
}

// isAtBottom returns true if the view is scrolled to (or near) the bottom.
func (m *AgentPaneModel) isAtBottom() bool {
	vis := m.VisibleLines()
	maxScroll := len(m.Lines) - vis
	if maxScroll <= 0 {
		return true
	}
	return m.ScrollOffset >= maxScroll-1
}

// rewrap derives wrapped Lines from RawLines for the current width.
func (m *AgentPaneModel) rewrap() {
	m.Lines = nil
	m.wrappedIndex = nil
	m.inCodeAfter = nil
	m.rawFenceAfter = nil
	m.invalidateMdCache()
	for _, raw := range m.RawLines {
		m.wrappedIndex = append(m.wrappedIndex, len(m.Lines))
		if m.width > 0 && runewidth.StringWidth(raw) > m.width {
			m.Lines = append(m.Lines, m.wrapLine(raw)...)
		} else {
			m.Lines = append(m.Lines, raw)
		}
	}
	m.recomputeCodeBlock(0)
}

func (m *AgentPaneModel) clampScroll() {
	maxScroll := len(m.Lines) - m.VisibleLines()
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.ScrollOffset > maxScroll {
		m.ScrollOffset = maxScroll
	}
}

// wrapLine wraps a single long line into multiple lines at word boundaries.
// Uses cell-width measurement to handle wide characters (CJK, emoji).
// Tracks remaining width incrementally to stay O(n) in line length.
func (m *AgentPaneModel) wrapLine(line string) []string {
	if m.width <= 0 {
		return []string{line}
	}

	// Precompute per-rune widths once
	runes := []rune(line)
	widths := make([]int, len(runes))
	for i, r := range runes {
		widths[i] = runewidth.RuneWidth(r)
	}

	var result []string
	start := 0

	for start < len(runes) {
		// Find how many runes fit within m.width cells
		cellW := 0
		fitEnd := start
		for i := start; i < len(runes); i++ {
			if cellW+widths[i] > m.width {
				break
			}
			cellW += widths[i]
			fitEnd = i + 1
		}

		// If everything remaining fits, we're done
		if fitEnd == len(runes) {
			break
		}

		if fitEnd == start {
			fitEnd = start + 1 // always make progress
		}

		// Try to break at a space within the second half
		breakAt := fitEnd
		for i := fitEnd - 1; i > start+(fitEnd-start)/2; i-- {
			if runes[i] == ' ' {
				breakAt = i + 1
				break
			}
		}

		result = append(result, string(runes[start:breakAt]))
		start = breakAt
	}

	result = append(result, string(runes[start:]))
	return result
}

// Clear clears the agent pane content and resets all transient state
// (selection, scroll, status, sanitizer) so no stale references survive
// into the next conversation.
func (m *AgentPaneModel) Clear() {
	m.RawLines = nil
	m.Lines = nil
	m.wrappedIndex = nil
	m.userRawLines = nil
	m.inCodeAfter = nil
	m.rawFenceAfter = nil
	m.invalidateMdCache()
	m.ScrollOffset = 0
	m.selActive = false
	m.selDragging = false
	m.selStartLn = 0
	m.selStartCol = 0
	m.cursorLn = 0
	m.cursorCol = 0
	m.status = event.StatusIdle
	m.sanitizer = sanitize.Sanitizer{}
	m.usage = usageState{}
	m.awaitingInput = nil
	if m.ModelSel.IsActive() {
		m.ModelSel.Close()
		m.recomputeInputLayout()
	}
}

// VisibleLines returns the number of content lines visible above the input area.
func (m *AgentPaneModel) VisibleLines() int {
	bottomH := m.inputHeight()
	if m.ModelSel.IsActive() {
		bottomH = m.modelSelHeight()
	}
	h := m.height - bottomH
	if h < 1 {
		h = 1
	}
	return h
}

func (m *AgentPaneModel) scrollToBottom() {
	vis := m.VisibleLines()
	if len(m.Lines) > vis {
		m.ScrollOffset = len(m.Lines) - vis
	} else {
		m.ScrollOffset = 0
	}
}

// SelectedRange returns the normalized (start, end) of the selection.
func (m *AgentPaneModel) SelectedRange() (int, int, int, int) {
	sl, sc := m.selStartLn, m.selStartCol
	el, ec := m.cursorLn, m.cursorCol
	if sl > el || (sl == el && sc > ec) {
		sl, sc, el, ec = el, ec, sl, sc
	}
	return sl, sc, el, ec
}

// SelectedText returns the text in the current selection.
func (m *AgentPaneModel) SelectedText() string {
	if !m.selActive || len(m.Lines) == 0 {
		return ""
	}
	sl, sc, el, ec := m.SelectedRange()
	if sl < 0 {
		sl = 0
		sc = 0
	}
	if el >= len(m.Lines) {
		el = len(m.Lines) - 1
		ec = utf8.RuneCountInString(m.Lines[el])
	}
	if sl == el {
		if sl >= len(m.Lines) {
			return ""
		}
		runes := []rune(m.Lines[sl])
		if sc > len(runes) {
			sc = len(runes)
		}
		if ec > len(runes) {
			ec = len(runes)
		}
		return string(runes[sc:ec])
	}

	var sb strings.Builder
	if sl < len(m.Lines) {
		runes := []rune(m.Lines[sl])
		if sc > len(runes) {
			sc = len(runes)
		}
		sb.WriteString(string(runes[sc:]))
		sb.WriteRune('\n')
	}
	for i := sl + 1; i < el && i < len(m.Lines); i++ {
		sb.WriteString(m.Lines[i])
		sb.WriteRune('\n')
	}
	if el < len(m.Lines) {
		runes := []rune(m.Lines[el])
		if ec > len(runes) {
			ec = len(runes)
		}
		sb.WriteString(string(runes[:ec]))
	}
	return sb.String()
}

// isUserLine returns true if the wrapped line index corresponds to a user
// message. Uses binary search on wrappedIndex (which is sorted by construction)
// so cost is O(log n) per call instead of O(n).
func (m *AgentPaneModel) isUserLine(wrappedIdx int) bool {
	if len(m.userRawLines) == 0 || len(m.wrappedIndex) == 0 {
		return false
	}
	// BinarySearch finds the insertion point for wrappedIdx+1.
	// The owning raw line is one before that.
	rawIdx, _ := slices.BinarySearch(m.wrappedIndex, wrappedIdx+1)
	rawIdx--
	if rawIdx < 0 {
		return false
	}
	return m.userRawLines[rawIdx]
}

func (m *AgentPaneModel) isSelected(line, col int) bool {
	if !m.selActive {
		return false
	}
	sl, sc, el, ec := m.SelectedRange()
	if line < sl || line > el {
		return false
	}
	if line == sl && col < sc {
		return false
	}
	if line == el && col >= ec {
		return false
	}
	return true
}

// invalidateMdCache clears the viewport markdown cache so the next Render
// recomputes it. Called when Lines content changes (AppendText, rewrap, Clear).
func (m *AgentPaneModel) invalidateMdCache() {
	m.mdCache = m.mdCache[:0]
	m.mdCacheOffset = -1
}

// cachedMarkdown returns the rendered markdown for Lines[lineIdx]. The cache
// is viewport-scoped: it holds at most VisibleLines() entries starting at
// m.ScrollOffset, so memory is O(visible) regardless of transcript length.
func (m *AgentPaneModel) cachedMarkdown(lineIdx int) string {
	vis := m.VisibleLines()

	// Rebuild cache if viewport moved or was invalidated.
	if m.mdCacheOffset != m.ScrollOffset || len(m.mdCache) != vis {
		if cap(m.mdCache) >= vis {
			m.mdCache = m.mdCache[:vis]
		} else {
			m.mdCache = make([]string, vis)
		}
		// Zero out entries (reused slice may have stale data).
		for j := range m.mdCache {
			m.mdCache[j] = ""
		}
		m.mdCacheOffset = m.ScrollOffset
	}

	slot := lineIdx - m.mdCacheOffset
	if slot < 0 || slot >= len(m.mdCache) {
		// Outside viewport — compute without caching.
		return renderMarkdownLine(m.Lines[lineIdx], m.isCodeLine(lineIdx), m.width)
	}
	if m.mdCache[slot] == "" {
		m.mdCache[slot] = renderMarkdownLine(m.Lines[lineIdx], m.isCodeLine(lineIdx), m.width)
	}
	return m.mdCache[slot]
}

// padLine pads or truncates a string to exactly width display cells.
// Uses cell-width measurement to handle wide characters (CJK, emoji).
func (m *AgentPaneModel) padLine(s string) string {
	w := runewidth.StringWidth(s)
	if w >= m.width {
		return runewidth.Truncate(s, m.width, "")
	}
	return s + strings.Repeat(" ", m.width-w)
}

// renderInputArea renders the textarea input into the output rows.
func (m *AgentPaneModel) renderInputArea(output []string, row *int) {
	inputRows := m.inputAreaEndRow - m.inputAreaStartRow
	if inputRows < 0 {
		inputRows = 0
	}
	renderedInput := m.input.Render()
	hasContent := m.inputActive || (len(renderedInput) > 0 && (len(renderedInput) > 1 || renderedInput[0].Text != ""))
	var cursorVisRow, cursorVisCol int
	if m.inputActive {
		cursorVisRow, cursorVisCol = m.input.CursorPosition()
	}

	for i := range inputRows {
		if *row >= m.height {
			break
		}
		visIdx := i + m.inputScrollOffset
		if hasContent && visIdx < len(renderedInput) {
			lineRunes := []rune(renderedInput[visIdx].Text)
			var lineBuilder strings.Builder
			cellsUsed := 0

			for j, r := range lineRunes {
				rw := runewidth.RuneWidth(r)
				if cellsUsed+rw > m.width {
					break
				}
				ch := string(r)
				if m.inputActive && visIdx == cursorVisRow && j == cursorVisCol {
					lineBuilder.WriteString(agentCursorStyle.Render(ch))
				} else if m.inputActive && m.input.IsSelected(visIdx, j) {
					lineBuilder.WriteString(agentSelStyle.Render(ch))
				} else {
					lineBuilder.WriteString(ch)
				}
				cellsUsed += rw
			}
			if m.inputActive && visIdx == cursorVisRow && cursorVisCol >= len(lineRunes) && cellsUsed < m.width {
				lineBuilder.WriteString(agentCursorStyle.Render(" "))
				cellsUsed++
			}
			if cellsUsed < m.width {
				lineBuilder.WriteString(strings.Repeat(" ", m.width-cellsUsed))
			}
			style := agentInputStyle
			if !m.inputActive {
				style = agentInputDim
			}
			output[*row] = style.Render(lineBuilder.String())
		} else if !hasContent && i == 0 {
			output[*row] = agentInputDim.Render(m.padLine(" Ctrl+G code | Alt+G plan"))
		} else {
			output[*row] = strings.Repeat(" ", m.width)
		}
		*row++
	}
}

// renderAPIKeyInput renders the API key input prompt.
func (m *AgentPaneModel) renderAPIKeyInput(output []string, row *int) {
	inputRows := m.inputAreaEndRow - m.inputAreaStartRow
	if inputRows < 1 {
		inputRows = 1
	}
	prompt := fmt.Sprintf(" API key for %s: ", m.apiKeyProfile)
	masked := strings.Repeat("*", len(m.apiKeyBuffer))
	cursor := agentCursorStyle.Render(" ")

	for i := range inputRows {
		if *row >= m.height {
			break
		}
		switch i {
		case 0:
			line := prompt + masked + cursor
			output[*row] = agentInputStyle.Render(m.padLine(line))
		case inputRows - 1:
			output[*row] = agentInputDim.Render(m.padLine(" Enter confirm · Esc cancel"))
		default:
			output[*row] = strings.Repeat(" ", m.width)
		}
		*row++
	}
}

// renderModelSelector delegates to the extracted ModelSelectorModel.
func (m *AgentPaneModel) renderModelSelector(output []string, row *int) {
	m.ModelSel.Render(output, row, m.width, m.height, m.inputAreaStartRow, m.inputAreaEndRow)
}

func (m *AgentPaneModel) renderStatusLine(style lipgloss.Style, statusMsg string) string {
	left := ""
	if m.modelLabel != "" {
		left = " " + sanitizeInlineDisplay(m.modelLabel)
	}
	// Append usage summary to the left section when data is available.
	if m.usage.turns > 0 && (m.usage.totalIn > 0 || m.usage.totalOut > 0) {
		sep := " "
		if left != "" {
			sep = " | "
		}
		prefix := "~"
		if m.usage.hasExact {
			prefix = ""
		}
		usage := fmt.Sprintf("%s%s%s in", sep, prefix, formatTokenCount(m.usage.totalIn))
		if m.usage.totalCached > 0 && m.usage.totalIn > 0 {
			usage += fmt.Sprintf(" (%s cached)", formatTokenCount(m.usage.totalCached))
		}
		usage += fmt.Sprintf(" · %s%s out", prefix, formatTokenCount(m.usage.totalOut))
		left += usage
	}
	right := ""
	if statusMsg != "" {
		right = statusMsg + " "
	}
	leftW := runewidth.StringWidth(left)
	rightW := runewidth.StringWidth(right)
	padding := m.width - leftW - rightW
	if padding < 1 {
		// Not enough space — prefer the status message over the model label.
		if right != "" {
			return style.Render(m.padLine(runewidth.Truncate(right, m.width, "…")))
		}
		return style.Render(m.padLine(runewidth.Truncate(left, m.width, "…")))
	}
	return style.Render(left + strings.Repeat(" ", padding) + right)
}

// Render renders the agent pane as exactly m.height lines joined by \n.
func (m *AgentPaneModel) Render() string {
	if m.height <= 0 || m.width <= 0 {
		return ""
	}

	output := make([]string, m.height)
	row := 0

	// Use package-level style vars directly — no local copies needed
	// since lipgloss styles are immutable value types.

	// No LLM configured — show message, but still render model selector if active.
	if !m.hasAgent {
		if row < m.height {
			output[row] = agentDimStyle.Render(m.padLine(""))
			row++
		}
		if row < m.height {
			output[row] = agentDimStyle.Render(m.padLine(" No LLM configured"))
			row++
		}
		if row < m.height {
			output[row] = agentDimStyle.Render(m.padLine(" Set LLM_API_KEY to enable"))
			row++
		}
		if m.ModelSel.IsActive() {
			bottomH := m.modelSelHeight()
			contentEnd := m.height - bottomH
			for row < contentEnd {
				output[row] = agentDimStyle.Render(m.padLine(""))
				row++
			}
			if row < m.height-1 {
				output[row] = agentDimStyle.Render(m.padLine(strings.Repeat("─", m.width)))
				row++
			}
			m.renderModelSelector(output, &row)
			for row < m.height {
				output[row] = agentDimStyle.Render(m.padLine(""))
				row++
			}
		} else {
			for row < m.height {
				output[row] = agentDimStyle.Render(m.padLine(""))
				row++
			}
		}
		return strings.Join(output, "\n")
	}

	// Content rows
	vis := m.VisibleLines()
	for i := range vis {
		if row >= m.height {
			break
		}
		lineIdx := m.ScrollOffset + i
		if lineIdx < len(m.Lines) {
			lineText := m.Lines[lineIdx]

			sl, _, el, _ := m.SelectedRange()
			if m.selActive && lineIdx >= sl && lineIdx <= el {
				// Render char-by-char with selection highlighting
				var line strings.Builder
				runes := []rune(lineText)
				cellsUsed := 0
				for j, r := range runes {
					w := runewidth.RuneWidth(r)
					if cellsUsed+w > m.width {
						break
					}
					ch := string(r)
					if m.isSelected(lineIdx, j) {
						line.WriteString(agentSelStyle.Render(ch))
					} else {
						line.WriteString(ch)
					}
					cellsUsed += w
				}
				// Pad remaining cells
				if cellsUsed < m.width {
					line.WriteString(strings.Repeat(" ", m.width-cellsUsed))
				}
				output[row] = line.String()
			} else if m.isUserLine(lineIdx) {
				output[row] = userMessageStyle.Render(m.padLine(lineText))
			} else {
				output[row] = m.cachedMarkdown(lineIdx)
			}
		} else {
			output[row] = strings.Repeat(" ", m.width)
		}
		row++
	}

	// Fill remaining content area
	bottomH := m.inputHeight()
	if m.ModelSel.IsActive() {
		bottomH = m.modelSelHeight()
	}
	contentEnd := m.height - bottomH
	for row < contentEnd {
		output[row] = strings.Repeat(" ", m.width)
		row++
	}

	// Separator line
	if row < m.height-1 { // -1 to leave room for status
		output[row] = agentDimStyle.Render(m.padLine(strings.Repeat("─", m.width)))
		row++
	}

	// Model selector and API key input replace the normal input area.
	if m.ModelSel.IsActive() {
		m.renderModelSelector(output, &row)
	} else if m.apiKeyInputActive {
		m.renderAPIKeyInput(output, &row)
	} else {
		m.renderInputArea(output, &row)
	}

	// Status line (last row): model label on the left, status on the right.
	if row < m.height {
		var statusMsg string
		style := agentStatusStyle
		switch m.status {
		case event.StatusIdle:
			statusMsg = "Ready"
			style = agentDimStyle
		case event.StatusThinking:
			statusMsg = "Thinking..."
		case event.StatusPlanning:
			statusMsg = "Planning..."
		case event.StatusPlanningWaiting:
			statusMsg = "Planning | :done to execute | :skip"
		case event.StatusReviewing:
			statusMsg = "Ctrl+O approve | Esc reject"
		case event.StatusEditing:
			statusMsg = "Ctrl+N to continue"
		case event.StatusWaiting:
			statusMsg = "Type to reply | Enter send"
		case event.StatusAwaitingInput:
			statusMsg = "Awaiting your answer | Enter send | Esc cancel"
			style = agentAwaitStyle
		case event.StatusTyping:
			statusMsg = "Agent typing... | Esc cancel"
		case event.StatusLinting:
			statusMsg = "Running style lint..."
		}
		output[row] = m.renderStatusLine(style, statusMsg)
	}

	return strings.Join(output, "\n")
}
