// Package pluginstore is the managed store for third-party plugins that
// nib imports from the Claude Code (CC) plugin ecosystem.
//
// nib does not read CC's on-disk layout (`.claude-plugin/`,
// `${CLAUDE_PLUGIN_ROOT}`) at runtime. Instead it imports a CC plugin
// once — fetch the upstream source, record where it came from and the
// exact commit it resolved to, and (in later milestones) convert it to
// nib-native artifacts — then tracks it as a managed install that can be
// re-synced when upstream changes. This package owns that store: the
// on-disk layout, the install registry that doubles as nib's first
// persisted settings file, and the fetch/track/re-sync lifecycle.
//
// This M0 foundation deliberately stops short of conversion: it proves
// the fetch → track → re-sync loop and the manifest/marketplace parsers.
// The CC→nib converter, loader integration, permission/trust model,
// hooks engine, and subagent engine land in later milestones (see the
// soul plan /nib/plans/cc-plugin-compat.md).
//
// Layout under the store root (see [Store]):
//
//	<root>/registry.json     — installed plugins + marketplaces (the ledger)
//	<root>/store/<id>/        — raw fetched upstream CC plugin
//	<root>/converted/<id>/    — nib-native output (populated in M1+)
//	<root>/data/<id>/         — persistent per-plugin data (${…_PLUGIN_DATA})
package pluginstore
