# nib

`nib` is the base agent: the default assembly of `kit/` blocks (memory, skills, `AGENTS.md`, MCP, extensions), put together by `kit/nib`. It is built entirely on `kit/`, `ai/`, and stdlib (MCP through the official Go SDK inside `kit/mcp`), with no `coding/`, `engine/`, or `tui/`. The depguard rule in `.golangci.yml` enforces that boundary.

If a kit consumer needs anything `coding/` provides, the kit boundary has a leak — fix kit, don't reach across.

## Usage

```sh
nib                     # start a session in the terminal
nib -m "<message>"      # run once on the given prompt
nib --list              # print the session index
nib --show <id>         # print a specific session's memory page
nib --plugins           # print the manifest (provider, store, context files, extensions, tools, MCP servers) and exit
nib --trust-mcp         # allow the MCP servers this project's .mcp.json names, and exit
nib --mcp-login <name>  # sign in to an HTTP MCP server in the browser, keep the sign-in, and exit
nib --approve           # start a session that asks before each command and file change
```

In a session, a line goes to the agent and its answer streams back, with one line per tool call. Ctrl-C stops a run, or leaves when nothing is running. A line typed while the agent works is queued as a follow-up. `/help` lists the commands: `/clear`, `/quit`, `/extension`, `/approve`, `/model` and `/usage`.

nib acts without asking by default; the shell still refuses in-place edits and destructive commands. In approve mode (`--approve`, or `/approve on` in a session) it asks before every call that may change something: shell commands, file changes, memory writes, skills and MCP tools. Reads are not asked about. Answer `y`, `a` (always, for the rest of the session) or `n`. A risky shell command runs only when you approve that call.

Long sessions are kept in bounds automatically. Tool output is sent whole once, then cut to its head; when the conversation is long, older output is reduced to a line (`NIB_TOOL_OUTPUT_CAP_DISABLED=1` turns both off). A response cut off at the output limit is retried with a higher limit. `--budget <tokens>` (or `NIB_TASK_TOKEN_BUDGET`) ends the session's runs once that many tokens are used.

`AGENTS.md` (or `CLAUDE.md`) files from the repository root down to the working directory are added to the system prompt as reference data, nearest last.

MCP servers come from `NIB_MCP` (`name=command args` or `name=https://endpoint`, separated by `;`) and from the project's `.mcp.json`. In `.mcp.json`, `command` is the program, taken whole, and `args` are its arguments; an HTTP server has `url` (and optionally `"type": "http"`, `headers`, and `oauth.clientId` for a client registered ahead of time). An HTTP endpoint must be https unless it is on this machine. The deprecated HTTP+SSE transport (`"type": "sse"`) is refused. Header values may use `${NAME}` from the environment, except in a project's file. A project's servers start only after `--trust-mcp`, and again after the file changes.

nib speaks MCP revision 2026-07-28 and falls back to 2024-11-05 through 2025-11-25 for older servers. A server's tools are named `mcp__<server>__<tool>`; its prompts are slash commands of the same form (`/mcp__<server>__<prompt> args`, the last argument taking the rest of the line); its resources are reached through `list_mcp_resources` and `read_mcp_resource`; its instructions join the system prompt. When a server's tools or prompts change, the session picks them up after the current step. A server that asks you something mid-call (elicitation) is answered in the session: a form field by field with a review before it is sent, or a page to open in the browser after a yes; without a session (`-m`) the question is dismissed.

An HTTP server that needs OAuth is signed in to once with `nib --mcp-login <name>`. The sign-in is kept in `<user-config-dir>/nib/mcp-auth.json` (readable only by you) and refreshed by later sessions; a session never opens a browser itself, and a server whose sign-in is missing or expired is reported with the command to run.

Extensions add tools. `workspace` (a shell, plus file and search tools confined to the project) is on by default. Switch them in `<user-config-dir>/nib/extensions.json` (`{"enable": [], "disable": []}`), per run with `--with` and `--without`, or in a session with `/extension on|off <name>`. The later level decides.

Optional flags:

- `--root <path>` — working directory for bash and demarkus root (default: cwd)
- `--debug` — write debug log to `<user-cache-dir>/<brand>/nib-debug.log`

## Memory layout

Sessions persist to demarkus under:

- `/nibster/index.md` — running index of all sessions, one line per run
- `/nibster/sessions/<session-id>.md` — per-session memory page authored by the agent

Session ID format: `YYYY-MM-DD-HHMMSS-<slug>-<8hex>` (UTC; slug derived from the message, lowercase alphanumeric, ~30 chars max; 8 hex chars of random entropy so same-second runs never collide).

The binary owns the index write (reliability — agents can run out of budget); the agent owns the session content (autonomy — that's the work product).

## Provider config

Resolved via `ai/llmconfig`:

1. Hardcoded defaults
2. `<UserConfigDir>/<brand>/llm.json` (global)
3. `<root>/.project/llm.json` (project-local)
4. Env vars (`LLM_API_KEY`, `LLM_BASE_URL`, `LLM_MODEL`, or profile-specific keys)

Same `llm.json` as `nib-code` and `nib-agent` — one global config across all three binaries.

## Exit codes

- `0` — success
- `1` — agent runtime error
- `2` — setup / configuration error (missing credentials, bad flags, etc.)
