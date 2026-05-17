# Agent Loop

`Run` + `Reply` schedule a single goroutine that drives the LLM stream and dispatches tools. Per-run state lives on `Agent` (`taskEdits`, `validatorRetries`, file cache, mode); the Approval Coordinator is allocated fresh at every `RunWithMode` / `Reply`-resume so stale signals from a prior run can never leak.

# Check For

- **Data races.** `FileCache` has its own mutex; access to per-run fields (`taskEdits`, `validatorRetries`) goes through `Agent.mu`.
- **Coordinator snapshot under lock.** The active `Coordinator` must be snapshotted under `Agent.mu` by signal methods (`Approve` / `Reject`) so a competing `RunWithMode` can't redirect a signal to a different run's channels.
- **Silent retry state reset.** Stateful tools must `Reset()` between runs — otherwise retry budgets from a prior task leak.
