# Edit Flow Orchestrator

`Orchestrator` runs the full review flow on top of a `Coordinator` (`kit/approval`). Same channel discipline applies — see `kit/approval/.greptile/rules.md` for the underlying contract.

# Check For

- **Orchestrator must thread the run's `*Coordinator` down through `Handle`** rather than reading agent state — passing it explicitly prevents a competing run from redirecting signals.
- **Channel sends are non-blocking** (`select` + `default`) — channels are buffered(1).
- **Reset must drain every channel** — re-used across tests.
