package wire

import (
	"testing"

	"github.com/latebit-io/nib/ai/brand"
)

func TestLoadMCPConfigsFromEnv(t *testing.T) {
	// loadMCPConfigs is the only testable unit without spawning real MCP servers.
	// Verify it correctly parses the brand-prefixed MCP env var into server configs.
	t.Setenv(brand.EnvKeyMCP, "team-wiki=echo hello;lsp-server=gopls")
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
	if got := configs["team-wiki"].Command; got != "echo hello" {
		t.Errorf("expected team-wiki command %q, got %q", "echo hello", got)
	}
	if got := configs["lsp-server"].Command; got != "gopls" {
		t.Errorf("expected lsp-server command %q, got %q", "gopls", got)
	}
}
