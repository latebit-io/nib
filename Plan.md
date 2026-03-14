# Realtime Collaborative Engineering — Plan

## Overview

Build a realtime collaborative coding tool between an engineer and an AI agent.
The agent and engineer work on the same file simultaneously — the agent proposes code changes
at specific locations, the engineer approves, edits, or rejects them inline, without breaking
flow or losing mental context.

The tool uses **[Micro editor](https://github.com/micro-editor/micro)** (Go TUI editor) as the
editing surface, a **Lua plugin** as the bridge, and a **Go sidecar process** as the agent loop.
No fork of Micro is required for this phase.

### Key Micro Plugin APIs

The Lua plugin uses these Micro APIs (see [plugins.md](https://github.com/micro-editor/micro/blob/master/runtime/help/plugins.md)):

| API | Purpose |
|-----|---------|
| `shell.JobSpawn(cmd, args, onStdout, onStderr, onExit)` | Start bridge binary |
| `shell.JobSend(cmd, data)` | Write JSON lines to bridge stdin |
| `buffer.NewBuffer(text, name)` with `BTScratch` | Agent pane (virtual, unsavable) |
| `buffer.Loc(col, line)` | 0-indexed position — **col first, then line** |
| `buf:Insert(loc, text)` / `buf:Remove(start, end)` | Apply ops to code buffer |
| `bp:VSplitIndex(buf, true)` | Open agent pane as right split |
| `micro.InfoBar():YNPrompt(msg, cb)` | Approve/reject prompt |
| `micro.InfoBar():Prompt(msg, default, type, cb)` | Redirect text input |
| `config.MakeCommand(name, func, completer)` | Register `:agent-start` command |

---

## Architecture

```
┌─────────────────────────────────────────────────┐
│  Micro Editor (existing binary)                  │
│                                                  │
│  ┌──────────────┐    ┌────────────────────────┐  │
│  │  Code pane   │    │  Agent pane (split)    │  │
│  │  (your file) │    │  (streaming log buf)   │  │
│  └──────────────┘    └────────────────────────┘  │
│                                                  │
│  ┌──────────────────────────────────────────┐    │
│  │  Lua plugin (~/.config/micro/plug/agent) │    │
│  │  - persistent connection via bridge      │    │
│  │  - receives pushed events (tokens, ops)  │    │
│  │  - applies EditOp to buffer              │    │
│  │  - shows approval prompt in infobar      │    │
│  │  - appends streaming tokens to log pane  │    │
│  │  - sends engineer messages back          │    │
│  └──────────────────────────────────────────┘    │
│                │ shell.JobStart                   │
│  ┌─────────────▼────────────────────────────┐    │
│  │  agent-bridge (Go binary)                │    │
│  │  - owned by plugin process lifetime      │    │
│  │  - relays JSON lines stdin ↔ socket      │    │
│  └─────────────┬────────────────────────────┘    │
└────────────────┼────────────────────────────────┘
                 │ Unix socket /tmp/junto-<pid>.sock
                 │ newline-delimited JSON (bidirectional)
┌────────────────▼────────────────────────────────┐
│  Go Agent Server                                 │
│                                                  │
│  - net.Listen("unix", ...) — no HTTP framework   │
│  - WorkingContext state machine                  │
│  - Claude API streaming                          │
│  - Fan-out: pushes events to all clients         │
│  - File I/O (reads/writes actual .go files)      │
└─────────────────────────────────────────────────┘
```

---

## IPC Protocol

Communication is **newline-delimited JSON over a Unix socket** — no HTTP, no web framework.
Each message is a single JSON object terminated by `\n`. Both sides can send at any time on
the same persistent connection. The Go standard library `net` package handles this directly.

### Server → Plugin (pushed events)

```json
{"type":"token","text":"I'll keep the interface minimal..."}
{"type":"step","index":2,"total":5,"description":"writing Verifier interface"}
{"type":"pending_op","op":{"id":"abc123","kind":"insert","line":4,"col":1,"text":"type Verifier interface {\n\tVerify(doc Document) (Result, error)\n}\n","reason":"Minimal interface — two methods cover the plan"}}
{"type":"approved","op_id":"abc123"}
{"type":"rejected","op_id":"abc123"}
{"type":"context","file":"verification.go","step":2,"total":5,"state":"pending"}
{"type":"error","message":"Claude API timeout"}
```

### Plugin → Server (engineer actions)

```json
{"type":"approve","op_id":"abc123"}
{"type":"reject","op_id":"abc123"}
{"type":"redirect","message":"add a ValidateWithContext method too"}
{"type":"edit","line":4,"col":1,"end_line":6,"end_col":1,"text":"// engineer changed this"}
{"type":"start","file":"verification.go","plan":["write Verifier interface","write Result struct","write impl"]}
```

The Go server fans out all push events to every connected client. Multiple clients
(e.g. a second terminal watching the session) get the same stream automatically.

---

## Repo Structure

```
protocol/                           -- shared Go module (message types, domain types)
  go.mod
  message.go                        -- flat message structs, Parse(), Marshal()
  types.go                          -- EditOp, Step, Edit, WorkingContext, AgentState

server/                             -- agent server Go module
  go.mod                            -- depends on protocol via replace directive
  cmd/junto-server/main.go          -- entry point, starts Unix socket server
  internal/
    socket/socket.go                -- net.Listen("unix"), client fan-out, message dispatch
    context/                        -- WorkingContext state machine (Phase 3+)

bridge/                             -- bridge binary Go module
  go.mod
  cmd/junto-bridge/main.go          -- relays JSON lines between stdin/stdout and socket

plugin/                             -- Lua plugin for Micro
  main.lua                          -- starts bridge, drives all editor interactions
  repo.json                         -- plugin metadata

Makefile                            -- builds all modules
```

---

## Core Data Types

### WorkingContext (Go)

```go
type WorkingContext struct {
    File        string
    Plan        []Step
    CurrentStep int
    State       AgentState
    PendingOp   *EditOp
    RecentEdits []Edit     // ring buffer, capped at 20, both sides
}

type AgentState string
const (
    StatePlanning    AgentState = "planning"
    StatePending     AgentState = "pending"      // awaiting engineer approval
    StateApplying    AgentState = "applying"     // op accepted, being written
    StateInterrupted AgentState = "interrupted"  // engineer redirected mid-step
)

type EditOp struct {
    ID      string `json:"id"`
    Kind    string `json:"kind"`              // "insert" | "replace" | "delete"
    Line    int    `json:"line"`
    Col     int    `json:"col"`
    EndLine int    `json:"end_line,omitempty"`
    EndCol  int    `json:"end_col,omitempty"`
    Text    string `json:"text"`
    Reason  string `json:"reason"`            // shown in agent pane before op arrives
}

type Step struct {
    Description string
    Done        bool
}

type Edit struct {
    Source  string // "agent" | "engineer"
    Line    int
    Col     int
    EndLine int
    EndCol  int
    Text    string
}
```

### Message format (Go)

Messages are flat JSON structs — no envelope wrapper. Each message type has its own struct
with a `type` discriminator field. `protocol.Parse([]byte)` reads the `type` field first,
then unmarshals into the correct struct. `protocol.Marshal(msg)` serializes + appends `\n`.

---

## Phases

### Phase 1 — Mechanical loop (no Claude) ✅ COMPLETE

**Goal:** Prove the full path works end-to-end: Go server → Unix socket → bridge →
Lua plugin → buffer insert → engineer approval → Go server receives it. No Claude yet.

**Go server tasks:**
- [x] `socket.go`: `net.Listen("unix", ...)`, accept connections in a goroutine, session-isolated path
- [x] Per-connection read loop: decode newline-delimited JSON from each client
- [x] Fan-out write loop: push newline-delimited JSON events to all connected clients
- [x] On new client connect: immediately push a hardcoded `pending_op` event
- [x] Handle incoming `approve` and `reject` messages — log and push confirmation event back

**Bridge binary tasks (`cmd/junto-bridge/main.go`):**
- [x] Connect to socket path (passed as CLI arg) on startup
- [x] Forward lines from stdin → socket
- [x] Forward lines from socket → stdout
- [x] Exit cleanly when stdin closes (plugin unloaded)

**Lua plugin tasks:**
- [x] `repo.json`: minimal plugin metadata
- [x] `main.lua`: on `init()`, register `:agent-start`, `:agent-stop`, `:agent-send` commands
- [x] Start bridge via `shell.JobSpawn("junto-bridge", {sock_path}, ...)` on `:agent-start`
- [x] Receive lines from bridge stdout, parse JSON, dispatch on `type` field
- [x] On `pending_op`: apply op to code buffer (insert/replace/delete), show YNPrompt
- [x] On approve: send `{"type":"approve","op_id":"..."}` to bridge, keep edit
- [x] On reject: undo edit, send `{"type":"reject","op_id":"..."}` to bridge
- [x] Inline JSON encode/decode (no external Lua deps, no dofile)

**Tests:**
- [x] `protocol/message_test.go`: 19 tests — Parse (all 12 types + 3 error cases), Marshal (4), RoundTrip (1)
- [x] `server/internal/socket/socket_test.go`: 6 tests — accept, dispatch, send, broadcast, disconnect, invalid JSON
- [x] `bridge/cmd/junto-bridge/main_test.go`: 3 tests — stdin→socket relay, socket→stdout relay, stdin close

**Status:** ✅ COMPLETE AND TESTED
- Go server + bridge verified end-to-end via CLI
- Lua plugin written, installed, and tested live in Micro
- Full loop works: server → socket → bridge → plugin → buffer insert → YNPrompt → approve/reject

**Test script:** `./test-phase1.sh`
- Automatically kills stale processes
- Starts server, displays socket path
- Waits for user input before launching Micro
- Run `agent-start <socket-path>` in Micro to connect
- Commands: `agent-start`, `agent-stop`, `agent-send`

---

### Phase 2 — Agent pane (streaming log)

**Goal:** Add the right-split agent pane that shows reasoning and the chat thread live.

**Go server tasks:**
- [ ] Push `token` events as text is generated (stub with a ticker for now)
- [ ] Push `step` events when moving between plan steps
- [ ] Push `context` events on state changes

**Lua plugin tasks:**
- [ ] On `init()`: open a vertical split with a virtual buffer named `[agent]`
- [ ] Set the split read-only, no line numbers, soft wrap enabled
- [ ] On `token` event: append text to the agent buffer
- [ ] On `step` event: append a formatted step header to the agent buffer
- [ ] On `pending_op` event: append a pending block to the agent buffer:
  ```
  ▸ step 2/5 — writing Verifier interface

  Agent: Keeping it minimal — two methods cover the plan.
         ValidateSchema takes raw bytes so callers don't
         need to pre-parse.

  ══ pending ══════════════════════════════════════
  lines 4–7   approve (y) · reject (n) · redirect (r)
  ═════════════════════════════════════════════════
  ```
- [ ] Auto-scroll agent pane to bottom on new content

**Done when:** Stub text streams into the right pane, the pending block renders correctly,
and the code pane shows the proposed insert at the right location simultaneously.

---

### Phase 3 — Claude integration

**Goal:** Wire the real Claude API into the agent loop.

**Go server tasks:**
- [ ] Add `github.com/anthropics/anthropic-sdk-go` dependency
- [ ] `agent.go`: build system prompt from WorkingContext — file content, plan, current
  step, recent edits from both sides
- [ ] Stream Claude's response token by token, pushing `token` events as they arrive
- [ ] Parse Claude's output for the structured op block — Claude is prompted to always
  end a proposal with a fenced block:
  ````
  ```op
  {"kind":"insert","line":4,"col":0,"text":"...","reason":"..."}
  ```
  ````
  Everything before the block streams as reasoning. When the closing fence is detected,
  parse the JSON, set `PendingOp`, transition state to `pending`, push `pending_op` event.
- [ ] On `approve`: write op text to file at the specified location, advance step, continue loop
- [ ] On `reject`: stay on current step, push `rejected` event, re-run with rejection in context
- [ ] On `redirect`: inject engineer message into Claude conversation history, re-run step
- [ ] Handle `start` message: initialise `WorkingContext`, begin agent loop

**Lua plugin tasks:**
- [ ] Bind `r` in agent pane context to open an infobar prompt for redirect input
- [ ] On redirect submit: write `{"type":"redirect","message":"..."}\n` to bridge stdin

**Done when:** Full loop works with real Claude — reasoning streams in, proposed code
appears in editor, approve/reject/redirect all function correctly end-to-end.

---

### Phase 4 — Edit detection (bidirectional awareness)

**Goal:** Engineer edits during a session are visible to the agent and influence its next turn.

**Lua plugin tasks:**
- [ ] Hook `onBufferModified` on the code pane buffer
- [ ] Debounce 250ms, then write `{"type":"edit",...}\n` with changed range + new text
- [ ] If manual edit overlaps the current `pending_op` range, auto-reject the pending op
  and write a `reject` message before the `edit` message

**Go server tasks:**
- [ ] Handle `edit` messages: append to `RecentEdits` ring buffer (cap 20)
- [ ] Detect overlap with `PendingOp` — if conflict, set state to `interrupted`, push event
- [ ] Include `RecentEdits` in Claude's context on next turn:
  `"Note: the engineer changed lines 4–6 to: [text]"`

**Done when:** You change a function signature mid-session and Claude's next message
explicitly acknowledges and adapts to it without being explicitly told.

---

## Key Design Decisions

### IPC: Unix socket, newline-delimited JSON, no HTTP

The Go server uses `net.Listen("unix", ...)` from the standard library — no web framework,
no HTTP, no dependencies. Each message is a complete JSON object on one line, flat (no envelope).
Socket path is session-isolated: `/tmp/junto-<pid>.sock` to prevent collisions between sessions.
This is the same pattern used by language servers (LSP), Docker's daemon, and most Unix tooling.

Why not HTTP: you need the server to push events to the client at any time (token streaming,
step changes, approval prompts). HTTP is request/response. SSE is a workaround on top of that.
A raw persistent socket connection is the correct primitive when both sides initiate messages.

Why not shared files or stdin/stdout on the server directly: shared files have polling races
and no push capability. Stdin/stdout works for single-client tools but breaks when the plugin
reconnects after a Micro restart, or when a second observer connects.

### Bridge binary: the Lua ↔ socket adapter

Lua in Micro cannot open Unix sockets. The bridge is a small Go binary (`agent-bridge`)
started by the plugin via `shell.JobStart`. It owns the socket connection and relays JSON
lines over its stdin/stdout. The plugin only sees a stream of lines — it doesn't know or
care about the socket. The bridge exits when the plugin is unloaded (stdin closes).

This keeps the plugin simple and the socket logic in Go where it belongs.

### Op granularity: one logical unit per checkpoint

The agent proposes one interface, one struct, or one function per checkpoint — never a
whole file, never a single line. The engineer evaluates each proposal in under 30 seconds.
If Claude wants to write more, it does so after the current op is approved. This is the
core of the interaction model.

### Prompt structure: fenced op block

Claude is instructed to separate reasoning from proposals. Reasoning streams freely.
The proposal is always a fenced ` ```op ` block at the end of the turn. The Go server
watches the stream for the opening and closing fences — content inside is parsed as
`EditOp` JSON. This gives a clean boundary without fragile heuristics on generated code.

### Agent pane: virtual buffer, plugin-owned

The agent pane is a Micro buffer with no backing file. The plugin owns it entirely —
appends to it, prevents accidental edits, controls scroll. Not saved to disk, discarded
when the session ends.

### No CRDT, last-write-wins

The Go server is the single source of truth for `WorkingContext`. Single-user, local tool.
CRDTs are for multi-user concurrent editing — not relevant here.

---

## Dependencies

**Go (agent server + bridge):**
```
github.com/anthropics/anthropic-sdk-go
```
Everything else (socket server, fan-out, JSON encoding, file I/O) uses the standard library.

**Lua plugin:**
- Standard Micro plugin APIs: `micro`, `micro/shell`, `micro/config`, `buffer`
- No external Lua libraries

**System:**
- `micro` installed (`brew install micro`)
- Agent server and bridge built and in PATH (`go build ./...`)
- `ANTHROPIC_API_KEY` set in environment

---

## Success Criteria

Done when all of the following work in a single session:

1. Open a `.go` file in Micro, run `:agent-start verification.go`
2. Agent pane opens in a right split showing the plan and current step
3. Claude streams reasoning into the agent pane token by token
4. A proposed code block appears highlighted in the editor at the correct line
5. Press `y` to approve — highlight becomes permanent, Claude advances to next step
6. Press `n` to reject — Claude acknowledges and proposes a revision
7. Press `r` and type a message — Claude adapts its next proposal to the redirect
8. Manually edit a line — Claude's next turn explicitly references the change

All 8 validate the core interaction model. If they work, the fork decision and further
investment are worth revisiting with real usage data.

---

## Out of Scope

- Multi-file sessions
- Persisting `WorkingContext` across restarts
- Any UI beyond the two-pane split + infobar prompts
- Remote agent servers or authentication
- Forking Micro (revisit only if the Lua layer proves genuinely limiting)
- LSP integration with the agent
