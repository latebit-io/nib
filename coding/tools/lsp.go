package tools

import "time"

// lspTimeout is the default timeout for LSP requests made by tools.
const lspTimeout = 10 * time.Second

// posArgs holds the JSON-decoded position arguments shared by LSP tools
// (go_to_definition, find_references).
type posArgs struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
}

// validatePosArgs returns an error string if the position arguments are
// invalid, or empty string on success.
func validatePosArgs(args posArgs) string {
	if args.Path == "" {
		return "Error: path is required"
	}
	if args.Line < 1 {
		return "Error: line must be >= 1 (1-indexed)"
	}
	if args.Col < 0 {
		return "Error: col must be >= 0"
	}
	return ""
}
