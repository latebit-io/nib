# Approval Coordinator

`Coordinator` owns the approve/reply channels between agent and frontend. Channel capacities are deliberately 1 and signal methods (`Approve` / `Reject` / `Reply`) are non-blocking `select` + `default` sends.

# Check For

- **Channel deadlocks.** `approveCh` and `inputCh` are buffered(1); sends MUST use `select` / `default` to avoid blocking.
- **Reset must drain every channel.** Coordinators are per-run, but tests reuse them.
