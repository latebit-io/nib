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
// What the converter handles today: slash commands, skills (shell-bearing
// ones keep their grants and run only once the plugin is trusted), agent
// definitions, command hooks, and stdio MCP servers, with import-time
// variables frozen. Anything it recognizes but cannot honor — non-stdio
// MCP transports, non-command hook types, unknown hook events, runtime
// variables — is recorded in the conversion report
// ([ConvertReport.Unsupported]) rather than dropped silently. Per-plugin
// trust ([InstalledPlugin.Trusted]) gates shell-bearing artifacts at load
// time in the consuming agent.
//
// Layout under the store root (see [Store]):
//
//	<root>/registry.json      — installed plugins + marketplaces (the ledger)
//	<root>/store/<id>/        — raw fetched upstream CC plugin
//	<root>/converted/<id>/    — nib-native output (commands/skills/.mcp.json)
//	<root>/data/<id>/         — persistent per-plugin data (${…_PLUGIN_DATA})
//	<root>/.marketplaces/<n>/ — fetched marketplace catalogs
package pluginstore
