package wire

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/coding/agent"
	codingtools "github.com/latebit-io/nib/coding/tools"
	"github.com/latebit-io/nib/kit/mcp"
)

// mcpServerConfig describes one MCP server in .mcp.json.
type mcpServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// mcpConfig is the top-level structure of .mcp.json (same format as Claude Code).
type mcpConfig struct {
	Servers map[string]mcpServerConfig `json:"mcpServers"`
}

// MCPResult holds the outputs of MCP server discovery.
type MCPResult struct {
	// Tools are the agent-compatible tool adapters from all MCP servers.
	Tools []agent.Tool
	// ServerNames lists the names of all successfully connected MCP servers.
	// The caller is responsible for interpreting these names (e.g. classifying
	// which servers are distributed memory based on naming conventions).
	ServerNames []string
	// Cleanup closes all MCP client connections. Must be called on shutdown.
	Cleanup func()
}

// DiscoverMCPTools connects to MCP servers and returns their tools as
// agent.Tool adapters. The caller must invoke MCPResult.Cleanup on shutdown.
func DiscoverMCPTools(projectRoot string) MCPResult {
	return connectMCPServers(loadMCPConfigs(projectRoot))
}

// DiscoverMCPToolsFromSources connects to MCP servers declared in the
// given converted plugin .mcp.json files. Each source's server names are
// namespaced with the plugin id (`<id>__<server>`) so two plugins that
// ship a server of the same name do not collide. The caller must invoke
// MCPResult.Cleanup on shutdown.
func DiscoverMCPToolsFromSources(sources []MCPConfigSource) MCPResult {
	merged := map[string]mcpServerConfig{}
	for _, src := range sources {
		data, err := os.ReadFile(src.Path)
		if err != nil {
			if !os.IsNotExist(err) {
				slog.Warn("mcp: read plugin config", "path", src.Path, "err", err)
			}
			continue
		}
		var cfg mcpConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			slog.Warn("mcp: invalid plugin config", "path", src.Path, "err", err)
			continue
		}
		for name, server := range cfg.Servers {
			merged[src.Prefix+"__"+name] = server
		}
	}
	return connectMCPServers(merged)
}

// MCPConfigSource points at one plugin's converted .mcp.json and the
// namespace prefix (the plugin id) to apply to its server names.
type MCPConfigSource struct {
	// Prefix namespaces the file's server names (typically the plugin id).
	Prefix string
	// Path is the converted .mcp.json file path.
	Path string
}

// connectMCPServers starts each configured server, performs the MCP
// handshake, and adapts the discovered tools. Shared by project and
// plugin discovery so both honor identical connect/cleanup semantics.
func connectMCPServers(configs map[string]mcpServerConfig) MCPResult {
	var tools []agent.Tool
	var serverNames []string
	var clients []*mcp.Client

	cleanup := func() {
		for _, c := range clients {
			_ = c.Close() // best-effort cleanup on shutdown
		}
	}

	if len(configs) == 0 {
		slog.Debug("mcp: no servers configured")
		return MCPResult{Cleanup: cleanup}
	}

	for name, cfg := range configs {
		// Split command into program + args if needed.
		cmdParts := strings.Fields(cfg.Command)
		if len(cmdParts) == 0 {
			slog.Warn("mcp: empty command for server", "name", name)
			continue
		}
		program := cmdParts[0]
		// Explicit allocation avoids aliasing cmdParts' backing array.
		args := make([]string, 0, len(cmdParts)-1+len(cfg.Args))
		args = append(args, cmdParts[1:]...)
		args = append(args, cfg.Args...)

		// Build environment. Config vars are appended after the parent
		// environment so they override existing values (subprocess uses
		// the last occurrence of a duplicate key).
		var env []string
		if len(cfg.Env) > 0 {
			env = os.Environ()
			for k, v := range cfg.Env {
				env = append(env, k+"="+v)
			}
		}

		client, err := mcp.NewStdioClient(program, args, env)
		if err != nil {
			slog.Warn("mcp: failed to start server", "name", name, "err", err)
			continue
		}

		serverTools, err := initMCPServer(client, name)
		if err != nil {
			_ = client.Close() // best-effort — close error irrelevant when init already failed
			slog.Warn("mcp: server setup failed", "name", name, "err", err)
			continue
		}
		clients = append(clients, client) // only track successfully initialized clients
		serverNames = append(serverNames, name)

		for _, info := range serverTools {
			adapted := codingtools.MCPToolInfo{
				Name:        info.Name,
				Description: info.Description,
				InputSchema: info.InputSchema,
			}
			tools = append(tools, codingtools.NewMCPToolAdapter(client, adapted))
			slog.Debug("mcp: registered tool", "server", name, "tool", info.Name)
		}
		slog.Info("mcp: connected", "server", name, "tools", len(serverTools))
	}

	return MCPResult{
		Tools:       tools,
		ServerNames: serverNames,
		Cleanup:     cleanup,
	}
}

// initMCPServer performs the MCP handshake and tool discovery for a single server.
func initMCPServer(client *mcp.Client, name string) ([]mcp.ToolInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.Initialize(ctx); err != nil {
		return nil, fmt.Errorf("initialize %s: %w", name, err)
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tools %s: %w", name, err)
	}
	return tools, nil
}

// loadMCPConfigs reads MCP server configurations from .mcp.json or the
// brand-prefixed MCP environment variable.
func loadMCPConfigs(projectRoot string) map[string]mcpServerConfig {
	// Try .mcp.json at project root (same location as Claude Code).
	configPath := filepath.Join(projectRoot, ".mcp.json")
	data, err := os.ReadFile(configPath)
	if err == nil {
		var cfg mcpConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			slog.Warn("mcp: invalid config", "path", configPath, "err", err)
		} else if len(cfg.Servers) > 0 {
			return cfg.Servers
		}
	}

	// Fall back to the brand-prefixed MCP env var: "name=command arg1 arg2"
	// Multiple servers separated by semicolons.
	mcpEnv := os.Getenv(brand.EnvKeyMCP)
	if mcpEnv == "" {
		return nil
	}

	configs := make(map[string]mcpServerConfig)
	for _, entry := range strings.Split(mcpEnv, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, cmd, found := strings.Cut(entry, "=")
		if !found || cmd == "" {
			slog.Warn("mcp: invalid env entry", "envvar", brand.EnvKeyMCP, "entry", entry)
			continue
		}
		configs[name] = mcpServerConfig{Command: cmd}
	}
	return configs
}
