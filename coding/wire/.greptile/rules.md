# Wire — Composition Layer

Composition layer shared by the TUI and headless binaries — wires LLM providers, memory servers, LSP, and MCP from environment + project config. **No business logic.** Must **NOT** import `tui/`. Constructors return errors; do not panic on missing config.
