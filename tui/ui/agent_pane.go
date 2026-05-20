package ui

import (
	"context"
	"fmt"
	"image/color"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/coding/event"
	kitcmd "github.com/latebit-io/nib/kit/command"
	tuicmd "github.com/latebit-io/nib/tui/command"
	"github.com/latebit-io/nib/tui/sanitize"
	"github.com/latebit-io/nib/tui/ui/textarea"
	"github.com/latebit-io/nib/tui/ui/theme"
	"github.com/mattn/go-runewidth"
)

// spinnerFrames are the braille animation glyphs cycled while the agent is
// actively working. Ten frames gives ~1s per full rotation at spinnerTickRate.
var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// spinnerTickRate is the cadence at which the spinner advances while active.
// 100ms keeps the cache warm and stays well under a full-frame redraw cost.
const spinnerTickRate = 100 * time.Millisecond

// spinnerTickMsg advances the agent pane's spinner frame. Scheduled by
// SetStatus on transition into an animated status, rescheduled from the
// pane's Update while the status remains animated, dropped when idle.
type spinnerTickMsg struct{}

// spinnerTickCmd schedules the next spinner advance.
func spinnerTickCmd() tea.Cmd {
	return tea.Tick(spinnerTickRate, func(_ time.Time) tea.Msg {
		return spinnerTickMsg{}
	})
}

// statusAnimates reports whether s is an "agent is actively working" state
// that should display the spinner glyph alongside its text. Passive states
// (idle, waiting for the developer, reviewing) never animate — the spinner
// is a signal of backgrounded activity, not a generic attention indicator.
func statusAnimates(s event.StatusKind) bool {
	switch s {
	case event.StatusThinking, event.StatusPlanning, event.StatusLinting:
		return true
	}
	return false
}

// statusStreaming reports whether s is a state during which the LLM is
// emitting tokens into the pane. Narrower than statusAnimates — linting
// runs in the engine and never produces AgentToken events, so it must
// not keep the streaming tint alive.
func statusStreaming(s event.StatusKind) bool {
	return s == event.StatusThinking || s == event.StatusPlanning
}

// Token-usage tracking (usageState, SetStreamingInput, UpdateUsage,
// UsageIndicator, ResetUsage, formatTokenCount, formatTurnStatsInline,
// formatCompacted, formatSessionSummary) lives in agent_pane_usage.go.

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
	// userMessageStyle paints the active beat's user-message body in the
	// nib accent so your turn stands out from agent prose. The leading
	// glyph cell ("◆ ", "⎙ ", "✕ ") is rendered separately by
	// [AgentPaneModel.renderUserLine] in its own per-kind hue.
	// The dim counterpart for past beats lives on [userMessageDimStyle] —
	// hue-preserving via [theme.AccentDim] so a past user message still
	// reads as a user message rather than fading into generic gray.
	userMessageStyle    = lipgloss.NewStyle().Foreground(theme.Accent).Bold(true)
	userMessageDimStyle = lipgloss.NewStyle().Foreground(theme.AccentDim).Bold(true)
	// agentDimStyle is the catch-all dim chrome color — past-beat prose
	// without a typed block, separators, "No LLM configured" splash,
	// fill rows. Anchored to the palette so dim chrome stays in family
	// with the rest of the redesign.
	agentDimStyle    = lipgloss.NewStyle().Foreground(theme.PrimaryTextDim)
	agentSelStyle    = lipgloss.NewStyle().Background(lipgloss.Color("24"))
	agentInputStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("230"))
	agentInputDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	agentCursorStyle = lipgloss.NewStyle().Reverse(true)
	// streamingTintStyle renders in-flight agent tokens with a slightly
	// muted foreground so the "live" portion of the transcript reads
	// differently from settled content. Markdown formatting is deferred
	// until the burst ends — it would flicker during token accumulation.
	streamingTintStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	// scrollbarTrackStyle and scrollbarThumbStyle render the right-edge
	// scroll indicator. Track uses a very dim ASCII bar; the thumb is a
	// slightly brighter heavy bar so the eye latches onto it.
	scrollbarTrackStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	scrollbarThumbStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))

	// Status chips — small colored labels on the right of the status line
	// communicating the agent's current state. Each chip has distinct
	// bg/fg so the eye locks on without reading text. Padding(0,1) gives
	// breathing room inside the color band.
	chipBase          = lipgloss.NewStyle().Padding(0, 1)
	chipStyleIdle     = chipBase.Background(lipgloss.Color("238")).Foreground(lipgloss.Color("245"))
	chipStyleThinking = chipBase.Background(lipgloss.Color("55")).Foreground(lipgloss.Color("231")).Bold(true)
	chipStylePlanning = chipBase.Background(lipgloss.Color("25")).Foreground(lipgloss.Color("231")).Bold(true)
	chipStylePlanWait = chipBase.Background(lipgloss.Color("60")).Foreground(lipgloss.Color("231")).Bold(true)
	chipStyleReview   = chipBase.Background(lipgloss.Color("136")).Foreground(lipgloss.Color("232")).Bold(true)
	chipStyleWaiting  = chipBase.Background(lipgloss.Color("89")).Foreground(lipgloss.Color("231")).Bold(true)
	chipStyleLinting  = chipBase.Background(lipgloss.Color("30")).Foreground(lipgloss.Color("231")).Bold(true)
	// chipStyleFinished marks "all tracked tasks complete" yields. Green
	// like editing-success but with a heavier weight so the developer
	// distinguishes "I'm done with the planned work" from the routine
	// REPLY pause without changing the input flow.
	chipStyleFinished = chipBase.Background(lipgloss.Color("22")).Foreground(lipgloss.Color("231")).Bold(true)

	// statusHintStyle renders the trailing keyboard hint next to a chip in
	// a dim color — it's context, not headline.
	statusHintStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))

	// statusLeftStyle renders the model/usage half of the status line in a
	// muted tone that reads as metadata.
	statusLeftStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
)

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

	// spinnerFrame is the index into spinnerFrames for the currently rendered
	// glyph. Advanced by spinnerTickMsg while statusAnimates(status) holds.
	spinnerFrame int
	// spinnerRunning is true while a spinner tick loop is in-flight. Prevents
	// SetStatus from scheduling duplicate ticks when an animated status
	// transitions to another animated status.
	spinnerRunning bool

	// turnCounter counts visible user exchanges. Starts at 1 for the implicit
	// initial turn (the first goal is Clear()'d off the pane, so the first
	// AppendUserMessage is turn 2). Incremented before emitting the separator.
	turnCounter int
	// turnStartRaw is the raw-line index of the first line of the current
	// turn (the separator line, or the "You:" line for the first turn).
	// Everything with a raw index below this watermark is rendered dim.
	turnStartRaw int

	// streamingStartRaw is the raw-line index where the current token
	// burst began, or -1 when no burst is in flight. Set on the first
	// AppendToken of a burst, cleared when SetStatus transitions away
	// from a streaming status. Lines at or above this index render with
	// streamingTintStyle instead of markdown.
	streamingStartRaw int

	// metaRawLines holds raw-line indices that should render as dim chrome
	// (tool calls, bracketed status updates, proposed/applied markers,
	// session summaries). Populated by AppendMeta.
	metaRawLines map[int]bool
	// plainRawLines holds raw-line indices whose content must bypass both
	// the markdown renderer and the code-fence detector — e.g. the
	// awaiting-input block, which carries attacker-supplied prompt text
	// that would otherwise be able to open a fence via unmatched
	// backticks and flip subsequent agent output into code styling.
	plainRawLines map[int]bool
	// turnSeparatorRawLines maps the raw-line index of a turn divider
	// to its label (e.g. "turn 2"). Render substitutes a full-width
	// centered rendering for these indices.
	turnSeparatorRawLines map[int]string

	// modelLabel is the display name of the active LLM model (e.g. "gemini-2.5-flash").
	// Shown on the left side of the status line. Set via SetModelLabel.
	modelLabel string

	// usage tracks cumulative token consumption for the status line display.
	usage usageState

	// pendingTurnUsage stashes the most recent AgentTurnUsage so the
	// stats can be folded into the next tool-call bullet (Option B
	// — one row per turn carrying both the tool name and its cost).
	// Consumed by [AppendToolCall] (folded into the bullet line) or
	// by [flushPendingTurnUsage] when a turn ends with no tool
	// (final assistant reply, AgentDone, or another AgentTurnUsage
	// arriving).
	pendingTurnUsage *event.AgentTurnUsage

	// pendingUserMessage holds a developer submission that arrived
	// while the agent was mid-stream. Render() shows a transient
	// "[queued: …]" banner row above the input area until the prior
	// turn's first tool-less AgentTurnUsage (or AgentWaiting/AgentDone
	// fallback) fires, at which point [FlushPendingUserMessage] commits
	// the queued text to the transcript via the normal turn-separator
	// + "You: …" flow. Empty when nothing is queued.
	//
	// Deferring the [AppendUserMessage] call is what keeps "You: …"
	// from being stamped in the middle of the prior turn's tail —
	// streaming tokens that continue to arrive after submission would
	// otherwise pile up below the user message, looking as if they
	// belonged to it.
	pendingUserMessage string

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

	// commandRegistry, when non-nil, intercepts SubmitMsg input that
	// parses as a slash command before it becomes a goal submission.
	// nil means slash dispatch is disabled and every submission goes
	// directly to GoalSubmittedMsg / PlanningGoalSubmittedMsg, the
	// pre-command behavior. Set via [SetCommandDispatch].
	commandRegistry *kitcmd.Registry

	// commandBusy is the optional probe Dispatch consults via
	// [kitcmd.WithBusyCheck]. Returns true when an agent turn is in
	// flight; the registry refuses dispatch in that window. nil
	// disables the busy check (commands always dispatch). Set via
	// [SetCommandDispatch].
	commandBusy func() bool

	// commandCtx is the context Dispatch threads through to handlers.
	// Typically the application's appCtx so a Ctrl+C / app shutdown
	// propagates to long-running command handlers (compaction,
	// history reset). nil falls back to context.Background — handlers
	// that wait on ctx.Done lose shutdown propagation but the
	// dispatch itself still runs. Set via [SetCommandDispatch].
	commandCtx context.Context //nolint:containedctx // long-lived dispatch ctx is the design

	// renderBuf is the per-frame output slice. Hoisted onto the model so
	// each Render call resizes/clears in place rather than allocating a
	// fresh []string. Capacity grows to the largest m.height seen.
	renderBuf []string

	// scrollTarget is the desired ScrollOffset value. During streaming,
	// the spinner tick loop advances ScrollOffset toward scrollTarget
	// via [advanceScrollEase] so auto-scroll-to-bottom slides smoothly
	// instead of jumping per-token. Outside streaming it equals
	// ScrollOffset since [scrollToBottom] snaps when no tick is in
	// flight to drive the ease.
	scrollTarget int

	// beats is the structured timeline of user → agent exchange cycles
	// (see agent_pane_beat.go). Step 0 of the agent-pane redesign — runs
	// alongside the existing line buffer and classification maps; later
	// steps will migrate the render path to walk beats directly.
	beats []Beat

	// userGlyphForRaw records the [UserGlyph] kind on the raw-line index
	// that carries the glyph prefix ("◆ ", "⎙ ", or "✕ "). Set by
	// AppendUserMessage at classify time; read by the render path so
	// the glyph cell can be styled in its own hue while the rest of
	// the user line keeps the uniform accent body color.
	userGlyphForRaw map[int]UserGlyph
}

// NewAgentPaneModel creates a new agent pane.
func NewAgentPaneModel(svc *Services, hasAgent bool) *AgentPaneModel {
	input := textarea.New(1) // width set properly in SetSize
	input.SetClipboard(svc.Clipboard)
	m := &AgentPaneModel{
		status:            event.StatusIdle,
		services:          svc,
		input:             input,
		hasAgent:          hasAgent,
		turnCounter:       1,
		streamingStartRaw: -1,
	}
	m.initBeats()
	return m
}

// --- Accessors ---

// SetStatus updates the agent status displayed in the status bar. Returns a
// tea.Cmd to start the spinner loop when transitioning from a non-animated
// state to an animated one (Thinking/Planning/Linting), otherwise nil. The
// loop stops itself when Update sees a tick after the status has left the
// animated set, so callers never need to cancel.
//
// Also settles any in-flight streaming burst: when the status leaves the
// token-producing set (Thinking/Planning), the tint watermark is dropped
// and the markdown cache is invalidated so settled lines rerender with
// full markdown styling.
func (m *AgentPaneModel) SetStatus(s event.StatusKind) tea.Cmd {
	m.status = s
	if !statusStreaming(s) && m.streamingStartRaw >= 0 {
		m.streamingStartRaw = -1
		m.invalidateMdCache()
	}
	if statusAnimates(s) && !m.spinnerRunning {
		m.spinnerRunning = true
		return spinnerTickCmd()
	}
	return nil
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
	return brand.Name
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
	case spinnerTickMsg:
		if !statusAnimates(m.status) {
			m.spinnerRunning = false
			// Streaming has ended — settle the scroll at the last
			// requested target so we land exactly at bottom rather
			// than mid-ease.
			m.ScrollOffset = m.scrollTarget
			return nil
		}
		m.spinnerFrame = (m.spinnerFrame + 1) % len(spinnerFrames)
		m.advanceScrollEase()
		return spinnerTickCmd()
	}
	return nil
}

// Streaming-text transcript pipeline (AppendToken,
// AppendTurnUsage, AppendMeta, AppendUserMessage,
// recomputeCodeBlock, parseFenceLine, isCodeLine) lives in
// agent_pane_transcript.go.

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
			// Empty Enter: drop focus and reset planning bit so the
			// next click doesn't silently submit as a plan.
			m.inputActive = false
			m.planningMode = false
			m.input.Reset()
			return nil
		}
		planning := m.planningMode
		m.inputActive = false
		m.input.Reset()
		m.planningMode = false
		return m.dispatchOrSubmit(text, planning)
	case textarea.CancelMsg:
		// Esc deactivates focus but preserves content — Ctrl+G or
		// click restores it. Clear planningMode so refocus via click
		// doesn't silently submit as a plan.
		m.inputActive = false
		m.planningMode = false
		return nil
	default:
		// Re-wrap the command — we already consumed the thunk.
		return func() tea.Msg { return result }
	}
}

// SetCommandDispatch wires (or unwires) slash-command dispatch into
// the input handler. After this call, SubmitMsg input that parses as
// a slash command runs through reg.Dispatch before falling back to
// goal submission. Pass reg=nil to disable.
//
// busy is consulted via [kitcmd.WithBusyCheck]. nil means the busy
// check is omitted — commands dispatch unconditionally. The binary
// typically wires busy to coding.Agent's parked-aware probe so
// commands cannot interleave with an in-flight LLM turn.
//
// ctx is threaded into [kitcmd.Registry.Dispatch] so handlers see
// program-shutdown cancellation. Pass nil to fall back to
// context.Background — acceptable for tests and fallback paths;
// production binaries should pass the application context.
func (m *AgentPaneModel) SetCommandDispatch(reg *kitcmd.Registry, busy func() bool, ctx context.Context) {
	m.commandRegistry = reg
	m.commandBusy = busy
	m.commandCtx = ctx
}

// dispatchOrSubmit is the post-validation tail of the SubmitMsg
// branch. With no registry wired, it preserves the pre-command
// behavior (emit goal- or planning-submitted message). With a
// registry, slash input is offered to Dispatch first; only
// non-matching input falls through.
//
// Return shape:
//   - matched=true, err=nil: command consumed the input. Returns nil
//     (or, if the command called SubmitPrompt, a Cmd that emits
//     GoalSubmittedMsg with the rendered prompt).
//   - matched=true, err!=nil: command failed. Surface the error
//     inline via AppendMeta and return nil. Goal submission is
//     suppressed — the user typed a slash command, even if it
//     errored, so falling through to the LLM would be surprising.
//   - matched=false: not a slash command. Behave as before (planning
//     vs. regular goal submission).
//
// Slash commands don't carry a planning-mode submission; the planning
// bit only applies on the goal-submission fallthrough path. A
// PromptCommand that wanted planning would need to be authored as a
// distinct command — keeps the contract narrow.
func (m *AgentPaneModel) dispatchOrSubmit(text string, planning bool) tea.Cmd {
	if m.commandRegistry != nil {
		sess := tuicmd.NewPaneSession(m)
		var opts []kitcmd.DispatchOption
		if m.commandBusy != nil {
			opts = append(opts, kitcmd.WithBusyCheck(m.commandBusy))
		}
		ctx := m.commandCtx
		if ctx == nil {
			ctx = context.Background()
		}
		matched, err := m.commandRegistry.Dispatch(ctx, sess, text, opts...)
		if matched {
			if err != nil {
				// Per the doc above: goal submission is suppressed for
				// any matched command, INCLUDING errored ones. A
				// HandlerCommand that called SubmitPrompt and then
				// returned an error must not silently leak its
				// pending prompt to the LLM — surface the error and
				// drop the prompt.
				m.AppendMeta(fmt.Sprintf("[%s]\n", err.Error()))
				return nil
			}
			if prompt, ok := sess.PendingPrompt(); ok {
				return func() tea.Msg { return GoalSubmittedMsg{Goal: prompt} }
			}
			return nil
		}
		// matched=false: input is not a slash command. Fall through.
	}
	if planning {
		return func() tea.Msg { return PlanningGoalSubmittedMsg{Goal: text} }
	}
	return func() tea.Msg { return GoalSubmittedMsg{Goal: text} }
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
	m.turnCounter = 1
	m.turnStartRaw = 0
	m.streamingStartRaw = -1
	m.metaRawLines = nil
	m.plainRawLines = nil
	m.turnSeparatorRawLines = nil
	m.spinnerFrame = 0
	// spinnerRunning intentionally not reset: a tick may still be in-flight
	// from before Clear(); Update drops it on the next fire because
	// status == StatusIdle.
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
	h := m.height - bottomH - m.pendingBannerHeight()
	if h < 1 {
		h = 1
	}
	return h
}

// pendingBannerHeight returns the row count reserved for the
// [pendingUserMessage] banner — 1 when a submission is queued and
// neither the model selector nor the API-key prompt is occupying the
// bottom area, else 0. Centralised so [VisibleLines] and Render stay
// in agreement about the content/banner split.
func (m *AgentPaneModel) pendingBannerHeight() int {
	if m.pendingUserMessage == "" {
		return 0
	}
	if m.ModelSel.IsActive() || m.apiKeyInputActive {
		return 0
	}
	return 1
}

// scrollToBottom requests an auto-scroll to the new bottom of the
// transcript. When the agent is actively streaming (statusAnimates),
// the existing spinner tick loop runs at [spinnerTickRate] and will
// advance ScrollOffset toward [scrollTarget] one step per frame via
// [advanceScrollEase] — turning what used to be a per-token jump into
// a continuous slide. Outside streaming we snap immediately because
// no tick is in flight to drive the ease.
//
// Direct ScrollOffset mutations from user input (mouse wheel, arrow
// keys, page up/down) stay instant — easing is only for auto-scroll on
// new content.
func (m *AgentPaneModel) scrollToBottom() {
	vis := m.VisibleLines()
	target := 0
	if len(m.Lines) > vis {
		target = len(m.Lines) - vis
	}
	m.scrollTarget = target
	if !statusAnimates(m.status) || !m.spinnerRunning {
		m.ScrollOffset = target
	}
}

// advanceScrollEase moves ScrollOffset one frame closer to scrollTarget.
// Critically damped at ~40% per frame so a 30-line jump settles in ~5
// frames at the 100ms spinner cadence (=500ms perceived ease). Snaps
// when within 2 lines so we never stall sub-line at the bottom.
func (m *AgentPaneModel) advanceScrollEase() {
	if m.ScrollOffset == m.scrollTarget {
		return
	}
	delta := m.scrollTarget - m.ScrollOffset
	const snap = 2
	if delta < 0 {
		if -delta <= snap {
			m.ScrollOffset = m.scrollTarget
			return
		}
	} else if delta <= snap {
		m.ScrollOffset = m.scrollTarget
		return
	}
	step := delta * 4 / 10
	if step == 0 {
		// Tiny delta but not within snap range — advance by one to
		// avoid stalling at +/- snap+1.
		if delta > 0 {
			step = 1
		} else {
			step = -1
		}
	}
	m.ScrollOffset += step
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

// rawIndexOf returns the raw-line index that owns the given wrapped line,
// or -1 if out of range. Uses binary search on wrappedIndex (sorted by
// construction) so cost is O(log n).
func (m *AgentPaneModel) rawIndexOf(wrappedIdx int) int {
	if len(m.wrappedIndex) == 0 {
		return -1
	}
	rawIdx, _ := slices.BinarySearch(m.wrappedIndex, wrappedIdx+1)
	rawIdx--
	if rawIdx < 0 {
		return -1
	}
	return rawIdx
}

// isUserLine returns true if the wrapped line index corresponds to a user
// message.
func (m *AgentPaneModel) isUserLine(wrappedIdx int) bool {
	if len(m.userRawLines) == 0 {
		return false
	}
	rawIdx := m.rawIndexOf(wrappedIdx)
	if rawIdx < 0 {
		return false
	}
	return m.userRawLines[rawIdx]
}

// isDim reports whether the wrapped line belongs to a previous turn and
// should render with dim styling. Everything at a raw index below the
// current turn's watermark is faded out so the current exchange stands out.
func (m *AgentPaneModel) isDim(wrappedIdx int) bool {
	if m.turnStartRaw <= 0 {
		return false
	}
	rawIdx := m.rawIndexOf(wrappedIdx)
	return rawIdx >= 0 && rawIdx < m.turnStartRaw
}

// isStreaming reports whether the wrapped line is part of the in-flight
// token burst and should render with streamingTintStyle. Returns false
// when no burst is active.
func (m *AgentPaneModel) isStreaming(wrappedIdx int) bool {
	if m.streamingStartRaw < 0 {
		return false
	}
	rawIdx := m.rawIndexOf(wrappedIdx)
	return rawIdx >= m.streamingStartRaw
}

// isMeta reports whether the wrapped line is chrome (tool calls, bracketed
// status, edit markers, session summaries) and should render dim.
func (m *AgentPaneModel) isMeta(wrappedIdx int) bool {
	if len(m.metaRawLines) == 0 {
		return false
	}
	rawIdx := m.rawIndexOf(wrappedIdx)
	return rawIdx >= 0 && m.metaRawLines[rawIdx]
}

// isPlain reports whether the wrapped line must bypass markdown and streaming
// styling. Used for agent-supplied content that could otherwise open code
// fences or trigger inline markdown (e.g. the awaiting-input block).
func (m *AgentPaneModel) isPlain(wrappedIdx int) bool {
	if len(m.plainRawLines) == 0 {
		return false
	}
	rawIdx := m.rawIndexOf(wrappedIdx)
	return rawIdx >= 0 && m.plainRawLines[rawIdx]
}

// turnSeparatorLabel returns the label for a wrapped line that represents a
// turn divider, or "" if it isn't one. Only the FIRST wrapped line of the
// owning raw line returns the label — continuations are handled by
// isTurnSeparatorContinuation so they render as dim blanks rather than
// falling through to markdown.
func (m *AgentPaneModel) turnSeparatorLabel(wrappedIdx int) string {
	if len(m.turnSeparatorRawLines) == 0 {
		return ""
	}
	rawIdx := m.rawIndexOf(wrappedIdx)
	if rawIdx < 0 {
		return ""
	}
	label, ok := m.turnSeparatorRawLines[rawIdx]
	if !ok {
		return ""
	}
	if rawIdx < len(m.wrappedIndex) && m.wrappedIndex[rawIdx] != wrappedIdx {
		return ""
	}
	return label
}

// isTurnSeparatorContinuation reports whether the wrapped line is a non-
// first wrapped segment of a separator raw line. This happens only at
// widths narrow enough to wrap the placeholder text — we blank those
// segments rather than let fragments of "── ♩ beat N ──" render as markdown.
func (m *AgentPaneModel) isTurnSeparatorContinuation(wrappedIdx int) bool {
	if len(m.turnSeparatorRawLines) == 0 {
		return false
	}
	rawIdx := m.rawIndexOf(wrappedIdx)
	if rawIdx < 0 {
		return false
	}
	if _, ok := m.turnSeparatorRawLines[rawIdx]; !ok {
		return false
	}
	return rawIdx < len(m.wrappedIndex) && m.wrappedIndex[rawIdx] != wrappedIdx
}

// overlayScrollbar paints the rightmost column of the content region with a
// track-and-thumb indicator when the transcript exceeds the viewport.
// Truncates each content row to width-1 cells (ANSI-aware) and appends the
// scrollbar cell, preserving styling on the truncated prefix.
//
// No-op when all lines fit — the column is left as content.
func (m *AgentPaneModel) overlayScrollbar(output []string, vis int) {
	total := len(m.Lines)
	if total <= vis || m.width <= 1 || vis <= 0 {
		return
	}

	// Proportional thumb. Keep at least one row visible so the user can
	// always see there is a scrollbar, even in very large transcripts.
	thumbStart := m.ScrollOffset * vis / total
	thumbEnd := (m.ScrollOffset + vis) * vis / total
	if thumbEnd <= thumbStart {
		thumbEnd = thumbStart + 1
	}
	if thumbEnd > vis {
		thumbEnd = vis
		if thumbStart >= thumbEnd {
			thumbStart = thumbEnd - 1
		}
	}

	for i := 0; i < vis; i++ {
		if i >= len(output) {
			break
		}
		var cell string
		if i >= thumbStart && i < thumbEnd {
			cell = scrollbarThumbStyle.Render("┃")
		} else {
			cell = scrollbarTrackStyle.Render("│")
		}
		output[i] = ansi.Truncate(output[i], m.width-1, "") + cell
	}
}

// renderTurnSeparator produces a full-width dim divider centered around the
// label, using heavy rules on each side. Falls back to a truncated label at
// narrow widths rather than overflowing the pane.
func renderTurnSeparator(label string, width int) string {
	if width <= 0 {
		return ""
	}
	text := " " + label + " "
	textW := runewidth.StringWidth(text)
	if textW+6 > width {
		// Not enough room for rules on both sides. Truncate the label
		// (with ellipsis) so it fits, then pad to width.
		if textW > width {
			text = runewidth.Truncate(text, width, "…")
			textW = runewidth.StringWidth(text)
		}
		return agentDimStyle.Render(text + strings.Repeat(" ", width-textW))
	}
	totalRules := width - textW
	leftRules := totalRules / 2
	rightRules := totalRules - leftRules
	line := strings.Repeat("─", leftRules) + text + strings.Repeat("─", rightRules)
	return agentDimStyle.Render(line)
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

// renderPendingBanner renders the single-row queued-message indicator
// shown between the transcript and the input area when
// [pendingUserMessage] is set. Newlines in the preview collapse to
// spaces so the banner reads as one line; padLine handles truncation
// at narrow widths.
func (m *AgentPaneModel) renderPendingBanner() string {
	preview := strings.TrimSpace(strings.ReplaceAll(m.pendingUserMessage, "\n", " "))
	return agentDimStyle.Render(m.padLine(" [queued: " + preview + "]"))
}

// inputStateHintLine returns the dim hint shown on the empty placeholder
// row beneath the textarea, picked per input state. Order of
// precedence: an active agent stream wins (the user can still queue,
// but the chrome should make the in-flight state obvious); then focus;
// then the legacy slash-command hint that anchored this row before
// the redesign.
//
// The spinner glyph comes from the same spinnerFrames array the
// status-line chip uses, so a single redraw tick advances both
// indicators in lock-step.
func (m *AgentPaneModel) inputStateHintLine() string {
	if statusAnimates(m.status) {
		spin := string(spinnerFrames[m.spinnerFrame%len(spinnerFrames)])
		return " " + spin + " nib is working… · esc interrupt"
	}
	if m.inputActive {
		return " enter send · shift+enter newline · esc cancel"
	}
	if m.input.Content() == "" {
		return " ask nib something…"
	}
	return " Ctrl+G code | Alt+G plan"
}

// inputStateDividerColor picks the color for the hairline divider
// directly above the input area. The divider is the cheapest place to
// signal input state without eating any textarea width or height — its
// hue tells you at a glance whether you're idle, focused, or watching
// the agent work.
func (m *AgentPaneModel) inputStateDividerColor() color.Color {
	if statusAnimates(m.status) {
		return theme.Warning
	}
	if m.inputActive {
		return theme.Accent
	}
	return theme.DividerActive
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
			output[*row] = agentInputDim.Render(m.padLine(m.inputStateHintLine()))
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

// statusChipSpec describes a chip's typography for a given status. Label is
// the short word that appears in the chip band; hint is the trailing
// keyboard-hint text rendered next to the chip in dim style.
type statusChipSpec struct {
	label string
	hint  string
	style lipgloss.Style
}

// chipFor returns the chip spec for the current status. When the status is
// animated the label is prefixed with the current spinner frame so the chip
// itself pulses rather than the spinner floating next to it.
func (m *AgentPaneModel) chipFor() statusChipSpec {
	var spec statusChipSpec
	switch m.status {
	case event.StatusIdle:
		spec = statusChipSpec{label: "READY", style: chipStyleIdle}
	case event.StatusThinking:
		spec = statusChipSpec{label: "THINKING", style: chipStyleThinking}
	case event.StatusPlanning:
		spec = statusChipSpec{label: "PLANNING", style: chipStylePlanning}
	case event.StatusPlanningWaiting:
		spec = statusChipSpec{label: "PLAN", hint: ":done execute · :skip", style: chipStylePlanWait}
	case event.StatusReviewing:
		spec = statusChipSpec{label: "REVIEW", hint: "Ctrl+O approve · Esc reject", style: chipStyleReview}
	case event.StatusWaiting:
		spec = statusChipSpec{label: "REPLY", hint: "Enter send", style: chipStyleWaiting}
	case event.StatusFinished:
		spec = statusChipSpec{label: "DONE", hint: "all tracked tasks complete · type to continue", style: chipStyleFinished}
	case event.StatusLinting:
		spec = statusChipSpec{label: "LINTING", style: chipStyleLinting}
	default:
		spec = statusChipSpec{label: "READY", style: chipStyleIdle}
	}
	if statusAnimates(m.status) {
		spec.label = string(spinnerFrames[m.spinnerFrame%len(spinnerFrames)]) + " " + spec.label
	}
	return spec
}

// renderStatusLine composes the final row: model + usage on the left, a
// colored status chip on the right with an optional dim hint. Falls back
// gracefully when the pane is narrow: drops the hint first, then the left
// section, then truncates the chip label.
func (m *AgentPaneModel) renderStatusLine() string {
	// Left section assembled in tiers — model label, per-turn stats,
	// context bar — so narrow panes degrade gracefully: drop the bar
	// first, then stats, then the whole left, instead of yanking
	// model+stats together the moment the bar overflows.
	hasUsage := m.usage.turns > 0 && (m.usage.totalIn > 0 || m.usage.totalOut > 0)

	modelChunk := ""
	if m.modelLabel != "" {
		modelChunk = " ◇ " + sanitizeInlineDisplay(m.modelLabel)
	}

	statsChunk := ""
	if hasUsage {
		prefix := "~"
		if m.usage.hasExact {
			prefix = ""
		}
		sep := " "
		if modelChunk != "" {
			sep = " · "
		}
		var b strings.Builder
		// Pi-style per-turn snapshot: ↑input ↓output R<cache> for
		// the LATEST turn. Matches pi's web-ui convention of
		// rendering each message's usage independently rather than
		// accumulating. Cumulative billing is surfaced in the
		// session-end summary, not the live footer.
		fmt.Fprintf(&b, "%s↑%s%s ↓%s%s",
			sep,
			prefix, formatTokenCount(m.usage.lastTurnFresh),
			prefix, formatTokenCount(m.usage.lastTurnOut))
		if m.usage.lastTurnCached > 0 {
			gross := m.usage.lastTurnFresh + m.usage.lastTurnCached
			if gross > 0 {
				pct := m.usage.lastTurnCached * 100 / gross
				fmt.Fprintf(&b, " R%s (%d%%⚡)",
					formatTokenCount(m.usage.lastTurnCached), pct)
			}
		}
		statsChunk = b.String()
	}

	barChunk := ""
	if hasUsage {
		// Bar visualization for context occupancy: ▓░ filled vs
		// empty cells + N%/<window> label. Sits at the tail of the
		// left section so it's the first thing dropped on narrow panes.
		if bar := formatContextBar(m.usage.lastTurnTotal, m.modelLabel); bar != "" {
			barChunk = " " + bar
		}
	}

	// Right: chip + optional hint.
	spec := m.chipFor()
	chip := spec.style.Render(spec.label)
	chipW := lipgloss.Width(chip)
	hint := ""
	hintW := 0
	if spec.hint != "" {
		hint = " " + statusHintStyle.Render(spec.hint) + " "
		hintW = lipgloss.Width(hint)
	}

	// Try left-section variants widest-first; for each, try with hint
	// and without. First combo whose total width fits wins. Variants
	// are emitted in decreasing-width order, so skipping a raw value
	// equal to the previous one filters duplicates (e.g. when barChunk
	// or statsChunk is empty) without a map allocation per frame.
	variants := [...]string{
		modelChunk + statsChunk + barChunk,
		modelChunk + statsChunk,
		modelChunk,
	}
	var prev string
	for _, raw := range variants {
		if raw == "" || raw == prev {
			continue
		}
		prev = raw
		styled := statusLeftStyle.Render(raw)
		w := lipgloss.Width(styled)
		if gap := m.width - w - chipW - hintW; gap >= 1 {
			return styled + strings.Repeat(" ", gap) + chip + hint
		}
		if gap := m.width - w - chipW; gap >= 1 {
			return styled + strings.Repeat(" ", gap) + chip
		}
	}
	// No left section (either all chunks empty or none fit). Still try
	// to keep the hint — for chips like REPLY/DONE/PLAN/REVIEW the
	// keyboard hint is the most actionable thing on the line.
	if gap := m.width - chipW - hintW; gap >= 1 {
		return strings.Repeat(" ", gap) + chip + hint
	}
	if gap := m.width - chipW; gap >= 0 {
		return strings.Repeat(" ", gap) + chip
	}
	// Chip itself doesn't fit — truncate the label (plain text) first so
	// runewidth can do it, then style.Render and finish with ANSI-aware
	// trim + pad. padLine is unsafe here because the input is styled and
	// its runewidth-based counter would miscount escape bytes.
	if m.width <= 2 {
		return strings.Repeat(" ", m.width)
	}
	fallback := spec.style.Render(runewidth.Truncate(spec.label, m.width-2, "…"))
	fW := lipgloss.Width(fallback)
	if fW > m.width {
		fallback = ansi.Truncate(fallback, m.width, "")
		fW = lipgloss.Width(fallback)
	}
	if fW < m.width {
		fallback += strings.Repeat(" ", m.width-fW)
	}
	return fallback
}

// Render renders the agent pane as exactly m.height lines joined by \n.
func (m *AgentPaneModel) Render() string {
	if m.height <= 0 || m.width <= 0 {
		return ""
	}

	if cap(m.renderBuf) < m.height {
		m.renderBuf = make([]string, m.height)
	} else {
		m.renderBuf = m.renderBuf[:m.height]
		clear(m.renderBuf)
	}
	output := m.renderBuf
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
			} else if label := m.turnSeparatorLabel(lineIdx); label != "" {
				// Separator check wins over dim so past-turn dividers
				// get the same full-width rule as the current one;
				// renderTurnSeparator already applies agentDimStyle.
				output[row] = renderTurnSeparator(label, m.width)
			} else if m.isTurnSeparatorContinuation(lineIdx) {
				// Wrapped tail of a separator placeholder at narrow
				// widths — blank dim row rather than letting the raw
				// fragment render as markdown garbage.
				output[row] = agentDimStyle.Render(strings.Repeat(" ", m.width))
			} else if blk, _, ok := m.blockAt(lineIdx); ok && hasLeftBorder(blk.Kind) {
				// Proposal/Error blocks render with a colored 2-cell
				// left border. Dimming preserves hue (theme.Dim) when
				// the line is in a past beat so structural information
				// (this beat errored / this proposal landed) survives
				// the fade rather than collapsing into generic gray.
				output[row] = m.renderBorderedLine(lineText, blk.Kind, m.isDim(lineIdx))
			} else if m.isUserLine(lineIdx) {
				// Typed user-message branch runs BEFORE the generic
				// isDim short-circuit so past user messages dim through
				// AccentDim (hue preserved) instead of fading to gray.
				// renderUserLine handles the glyph-color split on the
				// first wrapped line of an @-attached or interrupt
				// message; neutral messages render uniformly.
				output[row] = m.renderUserLine(lineText, lineIdx, m.isDim(lineIdx))
			} else if m.isDim(lineIdx) {
				output[row] = agentDimStyle.Render(m.padLine(lineText))
			} else if m.isMeta(lineIdx) {
				output[row] = agentDimStyle.Render(m.padLine(lineText))
			} else if m.isPlain(lineIdx) {
				output[row] = m.padLine(lineText)
			} else if m.isStreaming(lineIdx) {
				output[row] = streamingTintStyle.Render(m.padLine(lineText))
			} else {
				output[row] = m.cachedMarkdown(lineIdx)
			}
		} else {
			output[row] = strings.Repeat(" ", m.width)
		}
		row++
	}

	// Scrollbar overlay spans the rendered content rows only — must run
	// before the fill loop and separator/input rows consume the slot.
	m.overlayScrollbar(output, vis)

	// Fill remaining content area
	bottomH := m.inputHeight()
	if m.ModelSel.IsActive() {
		bottomH = m.modelSelHeight()
	}
	pendingH := m.pendingBannerHeight()
	contentEnd := m.height - bottomH - pendingH
	for row < contentEnd {
		output[row] = strings.Repeat(" ", m.width)
		row++
	}

	// Pending queued-message banner sits between the transcript and
	// the separator so the user sees immediate acknowledgement of a
	// mid-stream submission without disturbing the still-streaming
	// transcript above.
	if pendingH > 0 && row < m.height-1 {
		output[row] = m.renderPendingBanner()
		row++
	}

	// Separator line. Colored by input state so the hairline above the
	// textarea is the cheapest legible state indicator: dim gray when
	// idle, accent pink when focused, warning orange while the agent
	// is streaming. Eats no input width or height — the divider
	// already existed; we just route its color through state.
	if row < m.height-1 { // -1 to leave room for status
		dividerStyle := lipgloss.NewStyle().Foreground(m.inputStateDividerColor())
		output[row] = dividerStyle.Render(m.padLine(strings.Repeat("─", m.width)))
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

	// Status line (last row): model + usage on the left, colored chip
	// + optional keyboard hint on the right.
	if row < m.height {
		output[row] = m.renderStatusLine()
	}

	return strings.Join(output, "\n")
}
