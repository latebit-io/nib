// Package brand centralises the product's brand strings so the codebase
// can be rebranded by editing a single file. All user-facing strings —
// binary name suggestions, env-var prefix, config dir, OAuth originator,
// MCP server name, process label — derive from constants here. No other
// package may hardcode "nib" (or any successor brand) in its place; do
// the lookup via these constants instead.
package brand

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
