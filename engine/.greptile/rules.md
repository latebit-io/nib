# Engine — Hexagonal Core

Engine primitives layer (buffer, editor, event, lang, lsp, mcp, memory, highlight, project, validate). This is the hexagonal core — must have **ZERO** UI framework imports (no bubbletea, no lipgloss) and **ZERO** imports from `coding/` or `tui/`. Dependencies point inward only: buffer ← editor; events flow outward via the `engine/event` bus. Flag any import from `tui/`, `coding/`, or any UI framework.

# Key Contracts to Verify

- **Errors must be explicitly handled** (no `_ = fn()` without a comment explaining why).
- **Buffer operations must be grouped for undo** (`BeginGroup` / `EndGroup`).
- **Channel operations must not block indefinitely** — check for `ctx.Done()` paths.
