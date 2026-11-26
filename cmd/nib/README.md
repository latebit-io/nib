# nib

`nib` is the base agent: the default assembly of `kit/` blocks (memory, skills, `AGENTS.md`, MCP, extensions), put together by `kit/nib`. It is built entirely on `kit/`, `ai/`, and stdlib, with no `coding/`, `engine/`, or `tui/`. The depguard rule in `.golangci.yml` enforces that boundary.

If a kit consumer needs anything `coding/` provides, the kit boundary has a leak — fix kit, don't reach across.

## Usage

```sh
nib                     # start a session in the terminal
nib -m "<message>"      # run once on the given prompt
nib --list              # print the session index
nib --show <id>         # print a specific session's memory page
nib --plugins           # print the manifest (provider, store, context files, extensions, tools, MCP servers) and exit
nib --trust-mcp         # allow the MCP servers this project's .mcp.json names, and exit
```

In a session, a line goes to the agent and its answer streams back. Ctrl-C stops a run, or leaves when nothing is running. A line typed while the agent works is queued as a follow-up. `/help` lists the commands: `/clear`, `/quit`, and `/extension`.

`AGENTS.md` (or `CLAUDE.md`) files from the repository root down to the working directory are added to the system prompt as reference data, nearest last.

MCP servers come from `NIB_MCP` (`name=command args`, separated by `;`) and from the project's `.mcp.json`. In `.mcp.json`, `command` is the program, taken whole, and `args` are its arguments. A project's servers start only after `--trust-mcp`, and again after the file changes. Their tools are named `mcp__<server>__<tool>`.

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
