# Coding — Agent + Session Orchestration

Agent + session orchestration on top of engine primitives. Depends on `engine/` and `ai/llm`. Must **NOT** import `tui/` or any UI framework (no bubbletea, no lipgloss). The agent loop runs in its own goroutine; the TUI runs on the bubbletea goroutine.

# Cross-Cutting Rules

- Every error must be explicitly handled.
- All `Workspace` interface methods (`ProjectRoot`, `ReadFile`, `WriteFile`, `ListFiles`, `CanonPath` — composed from `FileReader` / `FileWriter`) are called from the agent goroutine and must not touch shared TUI state without synchronization. Same rule applies to any future method added to the Workspace surface.
- No package may import `tui/`.
