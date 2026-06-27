package lang

import (
	"path/filepath"
	"strings"
)

// languageMap maps file extensions to LSP language identifiers.
var languageMap = map[string]string{
	".go":    "go",
	".js":    "javascript",
	".mjs":   "javascript",
	".cjs":   "javascript",
	".jsx":   "javascriptreact",
	".ts":    "typescript",
	".tsx":   "typescriptreact",
	".py":    "python",
	".rs":    "rust",
	".rb":    "ruby",
	".java":  "java",
	".kt":    "kotlin",
	".c":     "c",
	".h":     "c",
	".cpp":   "cpp",
	".hpp":   "cpp",
	".cc":    "cpp",
	".cs":    "csharp",
	".swift": "swift",
	".lua":   "lua",
	".sh":    "shellscript",
	".bash":  "shellscript",
	".zsh":   "shellscript",
	".html":  "html",
	".css":   "css",
	".scss":  "scss",
	".json":  "json",
	".yaml":  "yaml",
	".yml":   "yaml",
	".toml":  "toml",
	".xml":   "xml",
	".sql":   "sql",
	".md":    "markdown",
	".zig":   "zig",
	".ex":    "elixir",
	".exs":   "elixir",
	".erl":   "erlang",
	".hs":    "haskell",
	".ml":    "ocaml",
	".mli":   "ocaml",
	".dart":  "dart",
	".r":     "r",
	".php":   "php",
	".vim":   "vim",
	".tf":    "terraform",
}

// DetectLanguage returns the LSP language identifier for a file path.
// go.mod and go.sum are matched on the full basename (not an extension) so a
// non-Go file like foo.mod is not mislabeled. All other languages are matched
// by extension. Returns empty string if unknown.
func DetectLanguage(path string) string {
	switch strings.ToLower(filepath.Base(path)) {
	case "go.mod":
		return "go.mod"
	case "go.sum":
		return "go.sum"
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return ""
	}
	return languageMap[ext]
}

// commentPrefixMap maps LSP language identifiers to their line comment prefix.
// Read-only after init — never mutate at runtime.
var commentPrefixMap = map[string]string{
	"go":              "//",
	"go.mod":          "//",
	"javascript":      "//",
	"javascriptreact": "//",
	"typescript":      "//",
	"typescriptreact": "//",
	"rust":            "//",
	"java":            "//",
	"kotlin":          "//",
	"c":               "//",
	"cpp":             "//",
	"csharp":          "//",
	"swift":           "//",
	"dart":            "//",
	"zig":             "//",
	"python":          "#",
	"ruby":            "#",
	"shellscript":     "#",
	"yaml":            "#",
	"toml":            "#",
	"elixir":          "#",
	"r":               "#",
	"php":             "//",
	"lua":             "--",
	"haskell":         "--",
	"sql":             "--",
	"erlang":          "%",
	"terraform":       "#",
	"vim":             "\"",
}

// LineCommentPrefix returns the line comment prefix for a file path,
// or empty string if the language has no line comment syntax (e.g. HTML, JSON).
func LineCommentPrefix(path string) string {
	lang := DetectLanguage(path)
	if lang == "" {
		return ""
	}
	return commentPrefixMap[lang]
}
