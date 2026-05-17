package prompts

import (
	"path/filepath"
	"strings"
)

// languageByExt maps lowercased file extensions (with leading dot) to a
// human-readable language name. Used by [DetectLanguage] to give the
// generating model a language anchor so it applies style rules using
// idioms native to the file's language instead of importing idioms
// from whichever language the rule wording resembles.
//
// Add new entries when a project surfaces a language we don't yet detect.
// Unknown or ambiguous extensions return "" — the caller treats that as
// "language unknown" and skips the language guidance section rather than
// guessing. Ambiguous extensions (e.g. .h, used by C, C++, and Obj-C with
// substantially different idioms) are intentionally absent.
var languageByExt = map[string]string{
	".go":     "Go",
	".lua":    "Lua",
	".py":     "Python",
	".pyi":    "Python",
	".js":     "JavaScript",
	".mjs":    "JavaScript",
	".cjs":    "JavaScript",
	".jsx":    "JavaScript",
	".ts":     "TypeScript",
	".tsx":    "TypeScript",
	".rs":     "Rust",
	".rb":     "Ruby",
	".java":   "Java",
	".kt":     "Kotlin",
	".kts":    "Kotlin",
	".swift":  "Swift",
	".c":      "C",
	".cpp":    "C++",
	".cc":     "C++",
	".cxx":    "C++",
	".hpp":    "C++",
	".hh":     "C++",
	".cs":     "C#",
	".scala":  "Scala",
	".clj":    "Clojure",
	".cljs":   "ClojureScript",
	".ex":     "Elixir",
	".exs":    "Elixir",
	".erl":    "Erlang",
	".hs":     "Haskell",
	".ml":     "OCaml",
	".mli":    "OCaml",
	".php":    "PHP",
	".pl":     "Perl",
	".pm":     "Perl",
	".sh":     "Shell",
	".bash":   "Shell",
	".zsh":    "Shell",
	".zig":    "Zig",
	".nim":    "Nim",
	".dart":   "Dart",
	".r":      "R",
	".jl":     "Julia",
	".sql":    "SQL",
	".vue":    "Vue",
	".svelte": "Svelte",
	".lisp":   "Common Lisp",
	".cl":     "Common Lisp",
	".scm":    "Scheme",
	".rkt":    "Racket",
	".fs":     "F#",
	".fsi":    "F#",
	".groovy": "Groovy",
	".gd":     "GDScript",
}

// DetectLanguage returns a human-readable language name inferred from
// path's extension, or "" when the extension is unknown. The result is
// safe to drop into prompt text directly (e.g. "This file is **Lua**.").
//
// Detection is extension-based only — no shebang or content sniffing —
// because callers always have a path and the prompt only needs a hint,
// not a definitive identification. A wrong guess is worse than no guess,
// so unknown extensions return "" rather than a fallback.
func DetectLanguage(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return ""
	}
	return languageByExt[ext]
}
