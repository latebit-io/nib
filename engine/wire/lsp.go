package wire

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/latebit-io/junto/engine/event"
	"github.com/latebit-io/junto/engine/lang"
	"github.com/latebit-io/junto/engine/lsp"
)

// lspServerConfig describes one LSP server in .project/lsp.json.
type lspServerConfig struct {
	Command    string   `json:"command"`
	Args       []string `json:"args"`
	Env        []string `json:"env"`
	LanguageID string   `json:"languageId"`
}

// LSPManager is the interface returned by InitLSP. It combines the
// capabilities both binaries need: document syncing (for file tracking,
// includes Close) and diagnostics (for the agent's DiagProvider).
type LSPManager interface {
	lang.DocumentSyncer
	lang.DiagnosticProvider
}

// InitLSP creates an LSP Manager from config or auto-detection.
// The events channel receives DiagnosticsUpdated events; the TUI renders
// them as an overlay while the headless runner ignores them. Both binaries
// benefit from the DiagProvider interface for agent diagnostics after edits.
// Returns nil if no language servers are configured or available.
func InitLSP(projectRoot string, events chan<- event.Event) LSPManager {
	configs := loadLSPConfigs(projectRoot)
	if len(configs) == 0 {
		configs = defaultLSPConfigs()
	}
	if len(configs) == 0 {
		return nil
	}
	return lsp.NewManager(configs, projectRoot, events)
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

	return configs
}
