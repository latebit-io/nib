# LLM Provider Abstraction

`Provider` is the interface (inbound port); `AgentAPI` is the adapter (OpenAI-compatible HTTP + SSE). Other adapters (Anthropic native, OAuth subscription) implement the same `Provider` surface.

# Check For

- **`ToolDef` types must stay exported** — used by `coding/tools` and `coding/agent`.
- **`Stream()` takes tools as a parameter** — tools must not be hardcoded into provider implementations.
- **SSE parsing must handle both `"data: "` (with space) and `"data:"` (without) prefix forms.**
- **Prompt-caching control blocks must not leak across providers that don't support them** — guard with provider capability flags.
