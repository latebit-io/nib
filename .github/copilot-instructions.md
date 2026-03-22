# Copilot Code Review Instructions

## Project Context

Junto is a pair-programming code editor with an AI agent. It is a Go monorepo with two modules:

- **`engine/`** — Frontend-agnostic library: buffer, editor, agent, session, LLM integration. Hexagonal architecture — no UI imports.
- **`tui/`** — Bubble Tea terminal frontend. Imports engine as a dependency.

## Go Version

This project uses **Go 1.26**. All Go 1.22+ features are valid:

- `range over int` — `for i := range n` is valid and idiomatic. Do NOT flag this as a compile error.
- `min`/`max` builtins — available since Go 1.21. Do NOT suggest `math.Min` or manual comparisons.
- `slices`, `maps`, `cmp` standard library packages — available and preferred over hand-rolled equivalents.

**Do not flag modern Go syntax as errors.** If you are unsure whether a construct compiles, assume the latest Go version supports it.

## Engine Architecture (Hexagonal)

The engine follows hexagonal (ports & adapters) architecture. The domain logic has zero knowledge of any frontend or infrastructure detail.

### Ports (Inbound)

These are the methods frontends call to drive the workflow:

- `session.SubmitGoal(goal)` — Start the agent with a developer intent.
- `session.ReviewEdit() *DiffResult` — Compute diff for a proposed edit. **Must** be called before ApproveEdit.
- `session.ApproveEdit(search, replace)` — Apply the reviewed edit. Fails if ReviewEdit was not called first.
- `session.RejectEdit()` — Reject the proposed edit.
- `session.Continue()` — Signal the agent to proceed after the developer edits.
- `session.CancelAgent()` — Cancel the active agent run.

### Ports (Outbound)

- `agent.Event` channel — The agent emits typed events (`TokenEvent`, `StatusEvent`, `EditProposedEvent`, `ErrorEvent`, `DoneEvent`) that the frontend reads and renders.
- `llm.Provider` interface — Abstracts the LLM API. Any implementation (OpenRouter, local, mock) satisfies this.

### Adapters

- `llm.AgentAPI` — Concrete adapter implementing `llm.Provider` via OpenAI-compatible HTTP API.
- `tui/` — Concrete adapter implementing the inbound port calls and outbound event rendering.

### Package Dependency Flow

```
session  →  agent  →  llm (Provider interface)
session  →  editor →  buffer
                   →  highlight (tree-sitter, internal to editor)
```

No cycles. All external dependencies are confined to `highlight` (tree-sitter CGo) and `llm` (net/http). The engine has no knowledge of Bubble Tea, lipgloss, or any terminal concept.

### Invariant Enforcement

The engine enforces domain rules that no frontend can bypass:

- **Two-step edit review**: `ApproveEdit` fails unless `ReviewEdit` was called first. Every frontend must show the developer the diff before applying.
- **Unexported fields**: `buffer.lines`, `agent.provider`, `session.agent`, `llm.apiKey` prevent invariant bypass (undo stack, mid-run provider swap, intent lifecycle skip).
- **State machine**: `HandleEvent` transitions session state. Frontends cannot manipulate `PendingEdit` or `editReviewed` directly.

## TUI Architecture (Bubble Tea / Elm)

The TUI follows the Elm architecture via Bubble Tea: `Model → Update → View` unidirectional data flow.

### Component Hierarchy

```
AppModel (root)
├── RegionManager          — Splits terminal into resizable panes
│   ├── EditorModel (pane) — Code editor with syntax highlighting
│   │   ├── *editor.Editor — Embedded engine editor (cursor, selection, scroll)
│   │   ├── DiffOverlay    — Inline diff preview with editable replacement
│   │   │   └── *editor.Editor — Overlay's own editor for replacement text
│   │   └── viewportMap[]  — Visual row → content type mapping for mouse
│   └── AgentPaneModel     — Agent output, status, input field
├── Session                — Engine session (domain logic)
└── KeyMap                 — Configurable keybindings
```

### Data Flow

1. **Input**: Bubble Tea delivers `tea.KeyMsg` / `tea.MouseMsg` to `AppModel.Update()`.
2. **Routing**: AppModel routes to the focused pane or handles global keys (approve, reject, submit goal).
3. **Domain**: Pane handlers call engine methods (`session.ApproveEdit`, `editor.MoveCursor`, etc.).
4. **Events**: Agent events arrive via channel, processed in `handleAgentEvent` which calls `session.HandleEvent` then renders in the agent pane.
5. **Render**: `AppModel.View()` calls each pane's `Render()`. The RegionManager composites them side-by-side.

### Editor Rendering

The editor render loop operates in **visual-line space** when a diff overlay exists:

- Lines 0..StartLine-1 → normal buffer lines
- Lines StartLine..EndLine → removed lines (red background, `-` gutter)
- Lines EndLine+1..EndLine+N → added overlay lines (green background, `+` gutter, editable)
- Lines EndLine+N+1.. → normal buffer lines (buffer index = visual - N)

`ScrollOffset` is in visual-line space. `ExtraVisualLines` on the engine editor accounts for the overlay lines in scroll math.

### Key Handler Architecture

`handleEditorKeyFor(keyMsg, e *editor.Editor, readOnly bool)` is the shared key handler. Both the main editor and the overlay editor use it — no duplication. The `readOnly` flag blocks mutations when the cursor is on removed lines or the selection overlaps the diff range.

## Review Focus — What Matters

Prioritize these categories (high to low):

1. **Correctness** — Logic errors, off-by-one, nil derefs, race conditions, deadlocks, channel misuse.
2. **Error handling** — Every error must be logged or surfaced. No silent `_ = fn()` without a comment explaining why.
3. **Concurrency safety** — Goroutine leaks, unguarded shared state, channel blocking. The agent runs in a goroutine and communicates via channels.
4. **Boundary violations** — Engine importing TUI types, frontend bypassing session methods, exported fields that leak invariants.
5. **Resource leaks** — Unclosed readers, tree-sitter parsers, HTTP response bodies.

## Performance

Flag these performance issues when they appear:

- **Allocation in hot paths**: The render loop runs every frame. Flag unnecessary allocations inside `Render()`, `renderNormalLine()`, `renderAddedLine()`, `renderRemovedLine()` — prefer reusing buffers, pre-allocating slices, and avoiding `fmt.Sprintf` where `strconv` or direct writes suffice.
- **Redundant reparse**: `HighlightLine()` calls `ReparseIfNeeded()` internally. Flag any code that calls `ReparseIfNeeded()` separately before `HighlightLine()` — it's a wasted check.
- **String/rune conversions**: `[]rune(string)` and `string([]rune)` allocate. Flag unnecessary round-trips, especially in loops. Prefer operating on `[]rune` throughout a function and converting once.
- **Map allocations**: `dispToBuf` and `charStyles` are allocated per-line per-frame in render methods. Flag opportunities to hoist them to the model level and reuse via `clear()` + reslice.
- **Channel backpressure**: The agent event channel is buffered (64). Flag any send path that could block the agent goroutine if the TUI is slow to drain events.
- **Tree-sitter lifecycle**: `highlight.Highlighter` owns a tree-sitter parser and tree. Flag any code path where `Close()` is not called when the editor is done (resource leak).

Do NOT flag:
- Micro-optimizations with no measurable impact (e.g., "use `strings.Builder` instead of `+` for 2-3 concatenations").
- Allocations outside hot paths (initialization, one-time setup, error paths).

## Security

Flag these security issues:

- **Command injection**: Any path where user input or LLM output reaches `exec.Command` or shell invocation without sanitization.
- **Path traversal**: The agent's `read_file` tool returns buffer content. Flag any code path where the agent could read arbitrary files outside the open buffer.
- **LLM prompt injection**: The agent receives user goals and file content. Flag any path where LLM-generated content is interpreted as commands, tool calls are executed without validation, or tool results bypass the PendingEdit approval flow.
- **Credential exposure**: `llm.apiKey` is unexported for a reason. Flag any code that logs, serializes, or surfaces API keys in error messages, debug output, or agent pane text.
- **Unbounded input**: Flag any buffer, string builder, or slice that grows without limit from external input (LLM streaming tokens, file content, user paste). The SSE scanner has a 10MB limit — flag similar patterns without bounds.
- **ANSI injection**: LLM output is sanitized before display. Flag any path where raw LLM text reaches the terminal without passing through the sanitizer.

Do NOT flag:
- The API key being in memory (it has to be).
- The LLM seeing file content (that's the product — pair programming requires it).

## Review Anti-Patterns — What to Avoid

Do NOT flag:

- **Idiomatic Go patterns as errors.** `range over int`, `min`/`max` builtins, bare returns with named results, short variable names in tight scopes.
- **Missing nil checks on internal code paths.** If a value is guaranteed non-nil by the calling context, a nil check is unnecessary. Only flag nil risks at system boundaries.
- **"Consider" suggestions with no concrete bug.** Every comment must identify a specific failure mode — what breaks, under what input, with what consequence. "Consider handling X" without a scenario is noise.
- **Style preferences.** Do not suggest renaming variables, adding comments to self-documenting code, or restructuring working code for aesthetic reasons.
- **Hypothetical future issues.** Review what the code does now, not what it might need to do later. Do not suggest adding extensibility, feature flags, or backwards-compatibility shims.
- **Behavior that is intentionally restricted.** Read-only removed lines, overlay deactivation on click-outside, blocking edits on removed-line ranges — these are deliberate UX decisions, not bugs.

## Code Patterns in This Codebase

These patterns are intentional. Do not flag them:

- **`EditorModel` embeds `*editor.Editor`** — Methods are called on the embedded editor. Fields like `m.CursorLine`, `m.Buf`, `m.ScrollOffset` come from the embedded struct.
- **ScrollOffset is in visual-line space when overlay exists.** The render loop maps visual lines to buffer lines or overlay lines. This is documented in the `Render()` comment.
- **`DiffOverlay.Editor` is a real `*editor.Editor`** wrapping a `buffer.Buffer` for the replacement text. This gives the overlay full editing capabilities (cursor, selection, undo) without duplication.
- **`handleEditorKeyFor(keyMsg, e, readOnly)`** is the shared key handler that operates on any `*editor.Editor`. Both main editor and overlay editor use it.
- **`clampScrollWithOverlay()`** and **`syncExtraVisualLines()`** bridge the engine's scroll math with the overlay's virtual lines.

## Tone

Be direct. Lead with the bug or risk, not a preamble. If the code is correct, say nothing — silence is approval.
