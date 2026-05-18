# Session Orchestration

`Session` orchestrates the developer-agent collaboration workflow.

# Key Invariants

- **Edit approval is three-step.** `ReviewEdit` (marks reviewed) → `PrepareApproval` (validates + returns plan; fails if not reviewed) → `CompleteApproval` / `AbortApproval`. No blind approvals.
- **All editor map keys must go through `CanonPath()`** for consistency.
- **`resolvePath()` must reject paths escaping `projectRoot`.**
- **`SwitchTo()` must NOT cancel the agent** — multi-file work continues across switches.
- **`editorForEdit()` auto-opens files from disk** if not already in the editors map.
- **`HandleEvent` must not block.** Status events are best-effort — session state must not depend on a status event arriving.
