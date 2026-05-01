// Package brand centralises the product's brand strings so the codebase
// can be rebranded by editing a single file. All user-facing strings —
// binary name suggestions, env-var prefix, config dir, OAuth originator,
// MCP server name, process label — derive from constants here. No other
// package may hardcode "nib" (or any successor brand) in its place; do
// the lookup via these constants instead.
package brand

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	// Name is the product's user-facing name. To rebrand, change this
	// and EnvPrefix below; everything else derives from Name.
	Name = "nib"

	// EnvPrefix is the namespace prefix for brand-owned environment
	// variables. Must equal strings.ToUpper(Name) + "_".
	EnvPrefix = "NIB_"

	// EnvKeyStyle selects an active style configuration by name.
	EnvKeyStyle = EnvPrefix + "STYLE"

	// EnvKeySmokeDisabled disables the smoke_run tool when set non-empty.
	EnvKeySmokeDisabled = EnvPrefix + "SMOKE_DISABLED"

	// EnvKeyValidatorsDisabled disables the post-edit validator pipeline
	// when set non-empty.
	EnvKeyValidatorsDisabled = EnvPrefix + "VALIDATORS_DISABLED"

	// EnvKeyMCP carries an inline MCP server config when no .mcp.json
	// file is present. Format: "name=command arg1 arg2;name2=...".
	EnvKeyMCP = EnvPrefix + "MCP"

	// EnvKeyCaptureDisabled disables capture-event recording when set
	// non-empty.
	EnvKeyCaptureDisabled = EnvPrefix + "CAPTURE_DISABLED"

	// ConfigDirName is the subdirectory under [os.UserConfigDir] where
	// persisted product config lives (style.json, llm.json, OAuth tokens).
	ConfigDirName = Name

	// ProcessLabel is the value used for process tagging (e.g. ps -ww
	// labelling) so external tooling can identify our processes.
	ProcessLabel = Name

	// OAuthOriginator is the originator app name sent to OAuth providers
	// (OpenAI, Anthropic) during the device-code / browser auth flow.
	OAuthOriginator = Name

	// MCPServerName is the server name this process advertises to MCP
	// clients during the initialise handshake.
	MCPServerName = Name

	// TempDirPrefix is the prefix for [os.MkdirTemp]-staged directories
	// used by the lint pipeline. Trailing dash is conventional for prefix
	// arguments to MkdirTemp.
	TempDirPrefix = Name + "-lint-"
)

// DebugLogPath resolves a debug-log path under the user's cache directory
// ([os.UserCacheDir]/[ConfigDirName]/<name>) and ensures the parent
// directory exists with mode 0700. The user-cache directory is a
// single-user trust boundary, which makes symlink-clobber attacks (a real
// concern with /tmp + O_TRUNC patterns) a non-issue without needing
// O_NOFOLLOW or random suffixes — we get a stable, predictable path the
// developer can `tail -F` across runs.
//
// name is the basename (e.g. "debug.log", "agent-debug.log"); callers
// should not include path separators.
func DebugLogPath(name string) (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("brand: resolve user cache dir: %w", err)
	}
	logDir := filepath.Join(cacheDir, ConfigDirName)
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return "", fmt.Errorf("brand: create log dir %s: %w", logDir, err)
	}
	return filepath.Join(logDir, name), nil
}
