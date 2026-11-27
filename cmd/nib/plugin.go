package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/kit/pluginstore"
)

// pluginStore opens the store of installed plugins.
func pluginStore() (*pluginstore.Store, error) {
	dir, err := brand.PluginsDir()
	if err != nil {
		return nil, err
	}
	return pluginstore.New(dir)
}

// pluginUsage lists what `nib plugin` does.
const pluginUsage = `usage: nib plugin <command>
  list                 show installed plugins
  install <source>     install from a path, owner/repo or git URL (enabled, not trusted)
  remove <id>          uninstall
  enable|disable <id>  switch a plugin on or off
  trust|untrust <id>   let a plugin's hooks, MCP servers and agents run, or stop them`

// runPlugin is `nib plugin ...`: it manages the installed plugins.
func runPlugin(args []string) error {
	store, err := pluginStore()
	if err != nil {
		return setupErr("plugin store: %v", err)
	}
	if len(args) == 0 {
		return setupErr("%s", pluginUsage)
	}
	command, rest := args[0], args[1:]
	if command == "list" {
		return listPlugins(store)
	}
	if len(rest) != 1 {
		return setupErr("%s", pluginUsage)
	}
	arg := rest[0]
	var done string
	switch command {
	case "install":
		return installPlugin(store, arg)
	case "remove":
		err, done = store.Remove(arg), "removed"
	case "enable":
		err, done = store.SetEnabled(arg, true), "on"
	case "disable":
		err, done = store.SetEnabled(arg, false), "off"
	case "trust":
		err, done = store.SetTrusted(arg, true), "trusted: its hooks, MCP servers and agents run from the next session"
	case "untrust":
		err, done = store.SetTrusted(arg, false), "no longer trusted"
	default:
		return setupErr("%s", pluginUsage)
	}
	if err != nil {
		return setupErr("%v", err)
	}
	fmt.Printf("%s: %s.\n", arg, done)
	return nil
}

// listPlugins prints each installed plugin and its state.
func listPlugins(store *pluginstore.Store) error {
	plugins := store.List()
	if len(plugins) == 0 {
		fmt.Println("No plugins are installed. Install one with: nib plugin install <source>")
		return nil
	}
	for _, p := range plugins {
		state := "off"
		if p.Enabled {
			state = "on"
		}
		trust := "not trusted"
		if p.Trusted {
			trust = "trusted"
		}
		fmt.Printf("  %-3s %s  %s, %s\n", state, p.ID, p.Version, trust)
	}
	return nil
}

// installPlugin installs and enables a plugin; its skills load at once,
// the rest once the person trusts it.
func installPlugin(store *pluginstore.Store, arg string) error {
	src, err := pluginstore.ParseSource(arg)
	if err != nil {
		return setupErr("%v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	p, err := store.Install(ctx, src, pluginstore.InstallOptions{Enabled: true})
	if err != nil {
		return setupErr("install: %v", err)
	}
	fmt.Printf("Installed %s %s. Its skills load in the next session.\n", p.ID, p.Version)
	// What the plugin brings that nib cannot use is said, not dropped.
	report, err := store.ImportReport(p.ID)
	if err != nil {
		warn("plugin import report", err)
	}
	for _, u := range report.Unsupported {
		fmt.Printf("  not used: %s %s: %s\n", u.Kind, u.Name, u.Reason)
	}
	fmt.Printf("Its hooks, MCP servers and agents run code; to allow them: nib plugin trust %s\n", p.ID)
	return nil
}
