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
	"strings"
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

	// EnvKeyToolOutputCapDisabled disables the per-tool-output cap when
	// set non-empty — older oversize tool results are not replaced with
	// the truncation marker, restoring uncapped baseline behaviour.
	// Kill switch for the cap; remove once smoke testing confirms the
	// cap is a net win on cache rate and billable tokens.
	EnvKeyToolOutputCapDisabled = EnvPrefix + "TOOL_OUTPUT_CAP_DISABLED"

	// EnvKeyBashApproval overrides the per-command bash approval default
	// (see [BashApprovalEnabled]): every bash command is proposed to the
	// frontend (AgentCommandProposed) and blocks until approved or
	// rejected. Interactive sessions default ON now that the TUI surface
	// ships; "0", "false", or "off" is the kill switch restoring
	// guard-only behaviour, any other non-empty value forces it on
	// (e.g. to opt a headless run into the per-command status trace).
	EnvKeyBashApproval = EnvPrefix + "BASH_APPROVAL"

	// EnvKeyTaskTokenBudget sets the per-task token budget cap (see
	// [coding/agent.NewOptions.TaskTokenBudget]). The cap is disabled by
	// default; set this to a positive integer to arm it (e.g. the value
	// of [kit/budget.RecommendedTaskTokens]). Unset, zero, or negative
	// leaves it disabled. A non-empty, non-integer value is rejected at
	// startup ([kit/budget.ParseEnvCap]) rather than silently ignored.
	EnvKeyTaskTokenBudget = EnvPrefix + "TASK_TOKEN_BUDGET"

	// EnvKeyPluginsDir overrides the managed plugin store root (default
	// <UserConfigDir>/<ConfigDirName>/plugins). Lets power users
	// relocate it and lets tests point it at a temp dir for hermetic
	// plugin-store coverage.
	EnvKeyPluginsDir = EnvPrefix + "PLUGINS_DIR"

	// EnvKeyGlobalSkillsDir overrides the user-global skills directory
	// (default <UserConfigDir>/<ConfigDirName>/skills). Lets power users
	// relocate it and lets tests point it at a temp dir for hermetic
	// global-layer coverage. Project-local skills (.project/skills) are
	// unaffected.
	EnvKeyGlobalSkillsDir = EnvPrefix + "GLOBAL_SKILLS_DIR"

	// EnvKeyGlobalAgentsDir overrides the user-global subagent-definitions
	// directory (default <UserConfigDir>/<ConfigDirName>/agents). Mirrors
	// EnvKeyGlobalSkillsDir; project-local agents (.project/agents) are
	// unaffected.
	EnvKeyGlobalAgentsDir = EnvPrefix + "GLOBAL_AGENTS_DIR"

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

// BashApprovalEnabled reports whether per-command bash approval is
// armed, combining [EnvKeyBashApproval] with the binary's default.
// Unset or empty defers to defaultOn; "0", "false", or "off"
// (case-insensitive) disables; any other value enables. Interactive
// binaries pass defaultOn=true (the TUI ships the approval surface);
// headless binaries pass false (their runner auto-approves, so arming
// only adds a per-command status trace).
func BashApprovalEnabled(defaultOn bool) bool {
	v := strings.TrimSpace(os.Getenv(EnvKeyBashApproval))
	if v == "" {
		return defaultOn
	}
	switch strings.ToLower(v) {
	case "0", "false", "off":
		return false
	}
	return true
}

// PluginsDir resolves the managed plugin store root:
// <UserConfigDir>/<ConfigDirName>/plugins (e.g.
// ~/.config/<brand>/plugins on Linux). [EnvKeyPluginsDir] overrides it
// verbatim when set non-empty. Returns an error only when the override
// is unset and the user config directory cannot be resolved.
func PluginsDir() (string, error) {
	if override := os.Getenv(EnvKeyPluginsDir); override != "" {
		return override, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("brand: resolve user config dir: %w", err)
	}
	return filepath.Join(dir, ConfigDirName, "plugins"), nil
}

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
