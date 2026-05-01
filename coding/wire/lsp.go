package wire

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/latebit-io/nib/coding/event"
	engineevent "github.com/latebit-io/nib/engine/event"
	"github.com/latebit-io/nib/engine/lang"
	"github.com/latebit-io/nib/engine/lsp"
)

// lspServerConfig describes one LSP server in .project/lsp.json.
type lspServerConfig struct {
	Command    string   `json:"command"`
	Args       []string `json:"args"`
	Env        []string `json:"env"`
	LanguageID string   `json:"languageId"`
}

// InitLSP creates an LSP Manager from config or auto-detection.
// The events channel receives DiagnosticsUpdated events; the TUI renders
// them as an overlay while the headless runner ignores them. Both binaries
// benefit from the DiagProvider interface for agent diagnostics after edits.
// Returns nil if no language servers are configured or available.
//
// engine/lsp emits engine/event.Event types; this function fans them into
// the application's coding/event.Event stream so frontends consume a single
// unified channel. The returned ServiceManager wraps the underlying
// [*lsp.Manager] so that Close() drains the manager AND closes the engine
// event channel — without that, the fan-in goroutine would leak (it ranges
// over a channel only the wire layer can close).
//
// The returned [lang.ServiceManager] is the canonical port; consumers
// import engine/lang for the type rather than reaching into wire.
func InitLSP(projectRoot string, events chan<- event.Event) lang.ServiceManager {
	configs := loadLSPConfigs(projectRoot)
	if len(configs) == 0 {
		configs = defaultLSPConfigs()
	}
	if len(configs) == 0 {
		return nil
	}
	engineEvents := make(chan engineevent.Event, 32)
	go fanInEngineEvents(engineEvents, events)
	return &lspWithCleanup{
		Manager:      lsp.NewManager(configs, projectRoot, engineEvents),
		engineEvents: engineEvents,
	}
}

// lspWithCleanup wraps [*lsp.Manager] so Close() also shuts the engine event
// channel that feeds [fanInEngineEvents]. The fan-in goroutine ranges over
// that channel — without an explicit close it would block forever after the
// LSP manager stops sending. Channel ownership stays with the wire layer
// (creator-closes convention); the engine package is unaware of the fan-in.
type lspWithCleanup struct {
	*lsp.Manager
	engineEvents chan engineevent.Event
	closeOnce    sync.Once
}

// Close shuts down the LSP manager (joining every server goroutine, so no
// more sends to engineEvents are in flight) and then closes the engine
// event channel so the fan-in goroutine exits cleanly. Idempotent via
// sync.Once — closing a channel twice would panic.
func (l *lspWithCleanup) Close() error {
	err := l.Manager.Close()
	l.closeOnce.Do(func() {
		close(l.engineEvents)
	})
	return err
}

// fanInEngineEvents translates editor-domain events from engine/event into
// the application's coding/event vocabulary so frontends consume a single
// channel. Drops events when the application channel is full — mirrors the
// drop-on-full semantics [*lsp.Manager] applies on its own send so
// shutdown cannot block this goroutine on a stalled consumer.
// Returns when the engine channel is closed (see [lspWithCleanup.Close]).
func fanInEngineEvents(in <-chan engineevent.Event, out chan<- event.Event) {
	for ev := range in {
		switch e := ev.(type) {
		case engineevent.DiagnosticsUpdated:
			select {
			case out <- event.DiagnosticsUpdated{Path: e.Path}:
			default:
				slog.Warn("wire: dropping diagnostics event (channel full)", "path", e.Path)
			}
		default:
			slog.Warn("wire: dropping unknown engine event", "type", ev)
		}
	}
}

// loadLSPConfigs reads .project/lsp.json from the project root.
func loadLSPConfigs(projectRoot string) []lsp.ServerConfig {
	path := filepath.Join(projectRoot, ".project", "lsp.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("lsp: cannot read .project/lsp.json", "err", err)
		}
		return nil
	}

	var raw struct {
		Servers map[string]lspServerConfig `json:"servers"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		slog.Warn("lsp: invalid .project/lsp.json", "err", err)
		return nil
	}

	configs := make([]lsp.ServerConfig, 0, len(raw.Servers))
	for name, cfg := range raw.Servers {
		cmdPath, err := exec.LookPath(cfg.Command)
		if err != nil {
			slog.Warn("lsp: configured server not found on PATH", "name", name, "command", cfg.Command)
			continue
		}
		configs = append(configs, lsp.ServerConfig{
			Command:    cmdPath,
			Args:       cfg.Args,
			Env:        cfg.Env,
			LanguageID: cfg.LanguageID,
		})
	}
	return configs
}

// defaultLSPConfigs auto-detects common language servers on PATH.
func defaultLSPConfigs() []lsp.ServerConfig {
	var configs []lsp.ServerConfig

	// gopls for Go
	if goplsPath, err := exec.LookPath("gopls"); err == nil {
		slog.Debug("lsp: auto-detected gopls", "path", goplsPath)
		configs = append(configs, lsp.ServerConfig{
			Command:    goplsPath,
			Args:       []string{"serve"},
			LanguageID: "go",
		})
	} else if !errors.Is(err, exec.ErrNotFound) {
		slog.Warn("lsp: unexpected error detecting gopls", "err", err)
	}

	// lua-language-server (sumneko) for Lua.
	if luaLSPath, err := exec.LookPath("lua-language-server"); err == nil {
		slog.Debug("lsp: auto-detected lua-language-server", "path", luaLSPath)
		configs = append(configs, lsp.ServerConfig{
			Command:    luaLSPath,
			LanguageID: "lua",
		})
	} else if !errors.Is(err, exec.ErrNotFound) {
		slog.Warn("lsp: unexpected error detecting lua-language-server", "err", err)
	}

	// yaml-language-server for YAML.
	if yamlLSPath, err := exec.LookPath("yaml-language-server"); err == nil {
		slog.Debug("lsp: auto-detected yaml-language-server", "path", yamlLSPath)
		configs = append(configs, lsp.ServerConfig{
			Command:    yamlLSPath,
			Args:       []string{"--stdio"},
			LanguageID: "yaml",
		})
	} else if !errors.Is(err, exec.ErrNotFound) {
		slog.Warn("lsp: unexpected error detecting yaml-language-server", "err", err)
	}

	return configs
}
