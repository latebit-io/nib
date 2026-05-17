# Review Tone

Be direct and technical. No pleasantries. Focus on correctness, concurrency safety, and architectural boundaries. Challenge code — disagreement backed by reasoning is expected. Flag silent error paths.

# Cross-Cutting Rules

These apply everywhere in this repository.

- **No silent errors.** Every error must be explicitly handled — logged or surfaced. No `_ = fn()` without a comment explaining why.
- **Doc comments on exported symbols.** Exported Go symbols (funcs, types, consts, vars, methods) need a doc comment beginning with the symbol name.
- **No hand-rolled stdlib.** Don't reimplement `slices.Sort`, `maps.Copy`, `strings.Cut`, etc. to avoid an import.

# Architectural Layering

The repository is multi-module. Import direction matters and is enforced by review:

- `ai/`, `engine/`, `kit/` are leaf modules with no nib intra-dependencies on `coding/` or `tui/`.
- `coding/` depends on `ai/`, `engine/`, `kit/` — but **must not** import `tui/` or any UI framework.
- `tui/` is the only module allowed to import `bubbletea` / `lipgloss`.
- `cmd/` binaries are the composition root — they wire the layers together.

Flag any import that violates these directions.

# Bootstrap Order (cmd/nib-code/main.go)

The TUI entry point's bootstrap is order-sensitive:

1. Resolve config + project root
2. Build editor + initial buffer
3. Construct Session (editor-only mode)
4. Construct Agent (via `coding/wire`) with Session as Workspace
5. Attach agent to session via `SetAgent()`

Session must exist before Agent — that breaks the circular Session-needs-agentPort vs Agent-needs-Workspace dependency. The auto-approval gate lives in `tui/ui/app_engine_events.go` (validator-pass → auto-apply, non-pass → surface overlay).
