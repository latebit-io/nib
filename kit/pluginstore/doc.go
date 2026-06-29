// Package pluginstore is the managed store for third-party plugins that
// nib imports from the Claude Code (CC) plugin ecosystem.
//
// nib does not read CC's on-disk layout (`.claude-plugin/`,
// `${CLAUDE_PLUGIN_ROOT}`) at runtime. Instead it imports a CC plugin
// once — fetch the upstream source, record where it came from and the
// exact commit it resolved to, convert it to nib-native artifacts — then
// tracks it as a managed install that can be re-synced when upstream
// changes. This package owns that store: the on-disk layout, the install
// registry that doubles as nib's first persisted settings file, the
// fetch/track/re-sync lifecycle, marketplace add/install, and the CC→nib
// converter.
//
// What the converter handles today: slash commands, prompt-only skills,
// and stdio MCP servers, with import-time variables frozen. Components it
// recognizes but cannot yet run — agents, hooks, shell-bearing skills,
// non-stdio MCP transports — are recorded in the conversion report
// ([ConvertReport.Unsupported]) rather than dropped silently; the runtime
// layers that execute them (permission/trust enforcement, hooks engine,
// subagent engine) land in later milestones. Per-plugin trust is recorded
// here ([InstalledPlugin.Trusted]) but not yet enforced. See the soul plan
// /nib/plans/cc-plugin-compat.md.
//
// Layout under the store root (see [Store]):
//
//	<root>/registry.json      — installed plugins + marketplaces (the ledger)
//	<root>/store/<id>/        — raw fetched upstream CC plugin
//	<root>/converted/<id>/    — nib-native output (commands/skills/.mcp.json)
//	<root>/data/<id>/         — persistent per-plugin data (${…_PLUGIN_DATA})
//	<root>/.marketplaces/<n>/ — fetched marketplace catalogs
package pluginstore
