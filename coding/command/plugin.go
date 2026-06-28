package command

import (
	"context"
	"fmt"
	"os"
	"strings"

	kitcmd "github.com/latebit-io/nib/kit/command"
	"github.com/latebit-io/nib/kit/pluginstore"
)

// PluginCommand is the `/plugin` handler: it manages the managed plugin
// store (install / update / remove / enable / disable / list) and the
// marketplaces plugins are installed from. It captures the store at
// construction; the store owns all persistence and conversion.
//
// Mutations that change which artifacts exist (install/update/remove/
// enable/disable) take effect on the next launch: nib loads plugin
// commands, skills, and MCP servers at startup, and the running agent's
// tool set is fixed for the session. The handler says so explicitly
// rather than implying a live reload it does not yet do.
type PluginCommand struct {
	store *pluginstore.Store
	def   kitcmd.Definition
}

// NewPlugin constructs the `/plugin` command over the given store.
func NewPlugin(store *pluginstore.Store) *PluginCommand {
	return &PluginCommand{
		store: store,
		def: kitcmd.Definition{
			Name:        "plugin",
			Description: "Manage imported Claude Code plugins (list|info|install|update|remove|enable|disable|trust|marketplace)",
			Source:      kitcmd.Source{Kind: kitcmd.SourceBuiltin},
		},
	}
}

// Definition implements [kitcmd.Command].
func (c *PluginCommand) Definition() kitcmd.Definition { return c.def }

// Handle implements [kitcmd.HandlerCommand].
func (c *PluginCommand) Handle(ctx context.Context, sess kitcmd.Session, args string) error {
	fields := strings.Fields(args)
	sub := "list"
	if len(fields) > 0 {
		sub = fields[0]
		fields = fields[1:]
	}
	switch sub {
	case "list", "ls":
		return c.handleList(sess)
	case "info":
		return c.handleInfo(sess, fields)
	case "install", "add":
		return c.handleInstall(ctx, sess, fields)
	case "update", "sync":
		return c.handleUpdate(ctx, sess, fields)
	case "remove", "rm", "uninstall":
		return c.handleRemove(sess, fields)
	case "enable":
		return c.handleEnable(sess, fields, true)
	case "disable":
		return c.handleEnable(sess, fields, false)
	case "trust":
		return c.handleTrust(sess, fields)
	case "marketplace", "mp":
		return c.handleMarketplace(ctx, sess, fields)
	default:
		return fmt.Errorf("unknown /plugin subcommand %q (try: list, info, install, update, remove, enable, disable, trust, marketplace)", sub)
	}
}

func (c *PluginCommand) handleList(sess kitcmd.Session) error {
	plugins := c.store.List()
	if len(plugins) == 0 {
		sess.Display("No plugins installed. Install one with /plugin install <path|owner/repo|git-url>")
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Installed plugins (%d):\n", len(plugins))
	for _, p := range plugins {
		state := "enabled"
		if !p.Enabled {
			state = "disabled"
		}
		trust := ""
		if p.Trusted {
			trust = " trusted"
		}
		fmt.Fprintf(&b, "  %s  [%s%s]  %s\n", p.ID, state, trust, p.Source.String())
		if rep, err := c.store.ImportReport(p.ID); err == nil {
			fmt.Fprintf(&b, "      %d commands, %d skills, %d mcp", len(rep.Commands), len(rep.Skills), len(rep.MCPServers))
			if n := len(rep.Unsupported); n > 0 {
				fmt.Fprintf(&b, "; %d unsupported (run /plugin info %s)", n, p.ID)
			}
			b.WriteString("\n")
		}
	}
	sess.Display(strings.TrimRight(b.String(), "\n"))
	return nil
}

func (c *PluginCommand) handleInfo(sess kitcmd.Session, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: /plugin info <id>")
	}
	id := args[0]
	p, ok := c.store.Get(id)
	if !ok {
		return fmt.Errorf("plugin %q not installed", id)
	}
	rep, err := c.store.ImportReport(id)
	if err != nil {
		return err
	}
	var b strings.Builder
	state := "enabled"
	if !p.Enabled {
		state = "disabled"
	}
	fmt.Fprintf(&b, "%s  [%s]  %s\n", p.ID, state, p.Source.String())
	fmt.Fprintf(&b, "Converted: %d commands, %d skills, %d mcp.\n", len(rep.Commands), len(rep.Skills), len(rep.MCPServers))
	writeUnsupported(&b, rep)
	sess.Display(strings.TrimRight(b.String(), "\n"))
	return nil
}

func (c *PluginCommand) handleInstall(ctx context.Context, sess kitcmd.Session, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: /plugin install <path|owner/repo|git-url>")
	}
	src, err := parseSource(args[0])
	if err != nil {
		return err
	}
	ent, err := c.store.Install(ctx, src, pluginstore.InstallOptions{Enabled: true})
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Installed %s (%s).\n", ent.ID, ent.Source.String())
	if rep, err := c.store.ImportReport(ent.ID); err == nil {
		fmt.Fprintf(&b, "Converted: %d commands, %d skills, %d mcp.\n", len(rep.Commands), len(rep.Skills), len(rep.MCPServers))
		writeUnsupported(&b, rep)
	}
	b.WriteString("Restart nib to load its commands, skills, and MCP servers.")
	sess.Display(b.String())
	return nil
}

func (c *PluginCommand) handleUpdate(ctx context.Context, sess kitcmd.Session, args []string) error {
	ids := args
	if len(ids) == 0 || ids[0] == "--all" {
		ids = nil
		for _, p := range c.store.List() {
			ids = append(ids, p.ID)
		}
	}
	if len(ids) == 0 {
		sess.Display("No plugins to update.")
		return nil
	}
	var b strings.Builder
	for _, id := range ids {
		changed, err := c.store.Update(ctx, id)
		if err != nil {
			fmt.Fprintf(&b, "  %s: error: %v\n", id, err)
			continue
		}
		if changed {
			fmt.Fprintf(&b, "  %s: updated\n", id)
		} else {
			fmt.Fprintf(&b, "  %s: up to date\n", id)
		}
	}
	b.WriteString("Restart nib to apply any updates.")
	sess.Display(b.String())
	return nil
}

func (c *PluginCommand) handleRemove(sess kitcmd.Session, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: /plugin remove <id>")
	}
	if err := c.store.Remove(args[0]); err != nil {
		return err
	}
	sess.Display(fmt.Sprintf("Removed %s. Restart nib to unload it.", args[0]))
	return nil
}

func (c *PluginCommand) handleEnable(sess kitcmd.Session, args []string, enabled bool) error {
	verb, past := "enable", "Enabled"
	if !enabled {
		verb, past = "disable", "Disabled"
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: /plugin %s <id>", verb)
	}
	if err := c.store.SetEnabled(args[0], enabled); err != nil {
		return err
	}
	sess.Display(fmt.Sprintf("%s %s. Restart nib to apply.", past, args[0]))
	return nil
}

func (c *PluginCommand) handleTrust(sess kitcmd.Session, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: /plugin trust <id>")
	}
	if err := c.store.SetTrusted(args[0], true); err != nil {
		return err
	}
	sess.Display(fmt.Sprintf("Trusted %s. Shell/hook execution from this plugin will be allowed once that layer ships (M2).", args[0]))
	return nil
}

func (c *PluginCommand) handleMarketplace(ctx context.Context, sess kitcmd.Session, args []string) error {
	if len(args) == 0 {
		return c.handleMarketplaceList(sess)
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			return fmt.Errorf("usage: /plugin marketplace add <path|owner/repo|git-url>")
		}
		src, err := parseSource(args[1])
		if err != nil {
			return err
		}
		mkt, err := c.store.AddMarketplace(ctx, src)
		if err != nil {
			return err
		}
		sess.Display(fmt.Sprintf("Added marketplace %q (%d plugins). Install with /plugin marketplace install %s <plugin>.", mkt.Name, len(mkt.Plugins), mkt.Name))
		return nil
	case "install":
		if len(args) < 3 {
			return fmt.Errorf("usage: /plugin marketplace install <marketplace> <plugin>")
		}
		ent, err := c.store.InstallFromMarketplace(ctx, args[1], args[2])
		if err != nil {
			return err
		}
		sess.Display(fmt.Sprintf("Installed %s from %s. Restart nib to load it.", ent.ID, args[1]))
		return nil
	case "list", "ls":
		return c.handleMarketplaceList(sess)
	default:
		return fmt.Errorf("unknown marketplace subcommand %q (try: add, install, list)", args[0])
	}
}

func (c *PluginCommand) handleMarketplaceList(sess kitcmd.Session) error {
	mkts := c.store.ListMarketplaces()
	if len(mkts) == 0 {
		sess.Display("No marketplaces added. Add one with /plugin marketplace add <path|owner/repo|git-url>")
		return nil
	}
	var b strings.Builder
	b.WriteString("Marketplaces:\n")
	for _, m := range mkts {
		fmt.Fprintf(&b, "  %s  %s\n", m.Name, m.Source.String())
	}
	sess.Display(strings.TrimRight(b.String(), "\n"))
	return nil
}

// writeUnsupported appends a compact summary of deferred components.
func writeUnsupported(b *strings.Builder, rep pluginstore.ConvertReport) {
	if len(rep.Unsupported) == 0 {
		return
	}
	fmt.Fprintf(b, "Not yet supported (%d):\n", len(rep.Unsupported))
	for _, u := range rep.Unsupported {
		fmt.Fprintf(b, "  - %s %s: %s\n", u.Kind, u.Name, u.Reason)
	}
}

// parseSource infers a plugin/marketplace source from a single CLI
// token. It expands a leading "~" and env vars, then prefers an existing
// local path (so a real relative dir like "plugins/demo" is not
// misclassified as an "owner/repo" GitHub slug). Otherwise: a URL
// ("://" or "git@") is git, a "." or "/" prefix is a local path, and a
// bare "owner/repo" is GitHub.
func parseSource(s string) (pluginstore.Source, error) {
	if s == "~" || strings.HasPrefix(s, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return pluginstore.Source{}, fmt.Errorf("resolve home dir: %w", err)
		}
		s = home + strings.TrimPrefix(s, "~")
	}
	// Expand env vars in all cases, including after ~ normalization, so
	// inputs like "~/plugins/$NAME" resolve fully.
	s = os.ExpandEnv(s)
	// An existing path wins outright — covers relative dirs that also look
	// like "owner/repo".
	if _, err := os.Stat(s); err == nil {
		return pluginstore.LocalSource(s), nil
	}
	switch {
	case strings.Contains(s, "://"), strings.HasPrefix(s, "git@"):
		return pluginstore.GitSource(s, ""), nil
	case strings.HasPrefix(s, "."), strings.HasPrefix(s, "/"):
		return pluginstore.LocalSource(s), nil
	case strings.Count(s, "/") == 1 && !strings.Contains(s, " "):
		return pluginstore.GitHubSource(s, ""), nil
	default:
		return pluginstore.Source{}, fmt.Errorf("cannot infer source from %q (use a path, owner/repo, or git URL)", s)
	}
}
