package wire

import "testing"

func TestMCPResultServerNamesPopulated(t *testing.T) {
	// loadMCPConfigs is the only testable unit without spawning real MCP servers.
	// Verify it correctly parses the JUNTO_MCP env var into server configs.
	t.Setenv("JUNTO_MCP", "team-wiki=echo hello;lsp-server=gopls")
	configs := loadMCPConfigs(t.TempDir())

	if len(configs) != 2 {
		t.Fatalf("expected 2 configs, got %d", len(configs))
	}
	if _, ok := configs["team-wiki"]; !ok {
		t.Error("expected team-wiki config")
	}
	if _, ok := configs["lsp-server"]; !ok {
		t.Error("expected lsp-server config")
	}
}
