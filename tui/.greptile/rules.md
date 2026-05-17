# TUI — Bubble Tea Frontend

Thin adapter around session + agent. Prism-style pane architecture (each pane implements `Update` / `Render` / `SetSize`).

# Check For

- **Business logic leaking into TUI.** Logic belongs in `coding/session` or `coding/agent`, not in `app.go` or pane code.
- **No imports from `coding/agent` internals** — only the public `Session` / `Agent` surface.
- **`RegionManager` handles layout** — panes should not calculate their own global positions.
- **`AppModel` is a thin adapter** — maps input → session methods, reads session state to render, adapts agent events to `tea.Msg`.
- **File switching must be blocked while `PendingEdit != nil`.**
- **`EditorModel` must be rebuilt when session auto-switches files.**
- **`lipgloss` styles must be hoisted to package-level vars** — never allocated per-frame in render methods.
