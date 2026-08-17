# nibster

`nibster` is the kit-boundary smoke test: a single-shot, general-purpose CLI agent built entirely on `kit/`, `ai/`, and stdlib — no `coding/`, `engine/`, or `tui/`. The depguard rule in `.golangci.yml` enforces that boundary.

If a kit consumer needs anything `coding/` provides, the kit boundary has a leak — fix kit, don't reach across.

## Usage

```sh
nibster -m "<message>"      # run an agent on the given prompt
nibster --list              # print the session index
nibster --show <id>         # print a specific session's memory page
nibster --plugins           # print the wired plug-in manifest (provider, store, tools) and exit
```

Optional flags:

- `--root <path>` — working directory for bash and demarkus root (default: cwd)
- `--debug` — write debug log to `<user-cache-dir>/<brand>/nibster-debug.log`

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
