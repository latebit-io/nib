package editor

// TokenKind identifies the syntactic role of a token.
// Frontends map these to their own styling (lipgloss, CSS, etc.).
//
// These types live in the editor package — not the highlight package —
// so binaries that never highlight (e.g. junto-agent) do not transitively
// link in tree-sitter grammar blobs.
type TokenKind string

const (
	// KindKeyword marks language keywords (if, for, func, etc.).
	KindKeyword TokenKind = "keyword"
	// KindString marks string literals and escape sequences.
	KindString TokenKind = "string"
	// KindComment marks comments.
	KindComment TokenKind = "comment"
	// KindNumber marks numeric literals.
	KindNumber TokenKind = "number"
	// KindType marks type names and type identifiers.
	KindType TokenKind = "type"
	// KindProperty marks object/mapping keys and struct fields.
	// Distinct from [KindType] because fields and type names are
	// semantically different (see nvim-treesitter's @property vs @type).
	KindProperty TokenKind = "property"
	// KindOperator marks arithmetic, comparison, and logical operators.
	// Brackets and delimiters are intentionally left unstyled so the output
	// isn't dominated by punctuation coloring.
	KindOperator TokenKind = "operator"
	// KindFunction marks function and method names at both definition
	// and call sites.
	KindFunction TokenKind = "function"
	// KindConstant marks constant identifiers and built-in constants
	// (true, false, nil, etc.).
	KindConstant TokenKind = "constant"
	// KindNone is the zero value — plain text with no syntax role.
	KindNone TokenKind = ""
)

// Token represents a highlighted range on a single line.
type Token struct {
	// Col is the start column (0-indexed, in runes).
	Col int
	// Len is the length in runes.
	Len int
	// Kind is the syntactic role of this token.
	Kind TokenKind
}

// Highlighter produces per-line syntax tokens for an editor buffer.
// Concrete implementations (e.g. tree-sitter-backed) live outside this
// package so the editor itself has no language-specific dependencies.
type Highlighter interface {
	// Parse parses the given source and caches tokens per line.
	// Called on initial set and whenever the buffer content changes
	// sufficiently that the cache should be rebuilt.
	Parse(source string)
	// HighlightLine returns cached tokens for the given line in O(1).
	HighlightLine(lineNum int) []Token
	// Close frees any resources held by the highlighter.
	// After Close, the Highlighter must not be used.
	Close()
}

// HighlighterFactory constructs a [Highlighter] for a given file path,
// or returns nil when the path has no supported language. Sessions that
// want syntax highlighting wire in a factory via [Session.SetHighlighterFactory];
// binaries that don't render (e.g. junto-agent) leave it unset.
type HighlighterFactory func(filename string) Highlighter
