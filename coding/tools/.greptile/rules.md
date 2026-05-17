# Tool Implementations

Tools depend on narrow collaborator interfaces (`FileReader`, `Approver`, `Navigator`, `FileCreator`, `TaskReviewer`) — **NEVER** on `Session` or `Agent` directly. Constructor injection only.

# Check For

- **No direct TUI / editor-buffer access.** Tools must not access TUI state or read editor buffers directly (would race against TUI-owned mutations); reads go through the `FileCache` (canonical-path keyed) or `Workspace.ReadFile` fallback.
- **No sensitive data in tool results.** No credentials, tokens, or full file contents from outside the project.
- **Cache keys are canonical absolute paths.** Every `cache.Set` / `Get` / `Invalidate` must use `CanonPath`, not the raw relative path.
