// Package highlight provides syntax highlighting via tree-sitter.
//
// Classification is driven by per-language tree-sitter highlight queries
// (`highlights.scm`) vendored alongside this package. Capture names follow
// the nvim-treesitter convention (e.g., `@keyword.function`, `@string.escape`)
// and are mapped to [TokenKind] via a prefix-fallback lookup: a name with
// no direct mapping falls back through its dotted prefixes.
package highlight

import (
	_ "embed"
	"log/slog"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/latebit-io/junto/engine/editor"
	tree_sitter_lua "github.com/tree-sitter-grammars/tree-sitter-lua/bindings/go"
	tree_sitter_yaml "github.com/tree-sitter-grammars/tree-sitter-yaml/bindings/go"
	sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_go "github.com/tree-sitter/tree-sitter-go/bindings/go"
)

//go:embed queries/go/highlights.scm
var goHighlightsSCM string

//go:embed queries/lua/highlights.scm
var luaHighlightsSCM string

//go:embed queries/yaml/highlights.scm
var yamlHighlightsSCM string

// Re-export the editor types so existing references to e.g. highlight.Token
// continue to resolve. Everything below is a thin alias — the canonical
// definitions live in the editor package so binaries that never render
// (junto-agent) can skip the tree-sitter grammar imports entirely.
type (
	// TokenKind aliases [editor.TokenKind].
	TokenKind = editor.TokenKind
	// Token aliases [editor.Token].
	Token = editor.Token
)

// Token-kind constants re-exported from the editor package. Use these
// aliases for readability inside highlighter internals; external callers
// can reference either `editor.KindKeyword` or `highlight.KindKeyword`.
const (
	KindKeyword  = editor.KindKeyword
	KindString   = editor.KindString
	KindComment  = editor.KindComment
	KindNumber   = editor.KindNumber
	KindType     = editor.KindType
	KindProperty = editor.KindProperty
	KindOperator = editor.KindOperator
	KindFunction = editor.KindFunction
	KindConstant = editor.KindConstant
	KindNone     = editor.KindNone
)

// langSpec describes one supported language: its tree-sitter grammar and
// the vendored highlight query.
type langSpec struct {
	lang *sitter.Language
	scm  string
}

// langByExt maps file extensions to the language spec used to highlight them.
// Unsupported extensions produce a nil [Highlighter] from [New].
var langByExt = map[string]langSpec{
	".go":   {sitter.NewLanguage(tree_sitter_go.Language()), goHighlightsSCM},
	".lua":  {sitter.NewLanguage(tree_sitter_lua.Language()), luaHighlightsSCM},
	".yaml": {sitter.NewLanguage(tree_sitter_yaml.Language()), yamlHighlightsSCM},
	".yml":  {sitter.NewLanguage(tree_sitter_yaml.Language()), yamlHighlightsSCM},
}

// captureNameToKind maps tree-sitter capture names (nvim-treesitter convention)
// to [TokenKind]. Lookup is prefix-fallback: if "keyword.function" is absent,
// we try "keyword" before giving up. Unmapped names produce [KindNone] and
// are skipped during rendering.
var captureNameToKind = map[string]TokenKind{
	"keyword":     KindKeyword,
	"conditional": KindKeyword,
	"repeat":      KindKeyword,
	"label":       KindKeyword,
	"include":     KindKeyword,
	"exception":   KindKeyword,
	"preproc":     KindKeyword,
	"attribute":   KindKeyword,
	"property":    KindProperty,
	"string":      KindString,
	"escape":      KindString,
	"comment":     KindComment,
	"number":      KindNumber,
	"boolean":     KindConstant,
	"constant":    KindConstant,
	"type":        KindType,
	"constructor": KindType,
	"operator":    KindOperator,
	"function":    KindFunction,
	"method":      KindFunction,
}

// kindForCaptureName resolves a capture name to a [TokenKind] using
// prefix-fallback: "string.escape" → "string.escape", "string", KindNone.
func kindForCaptureName(name string) TokenKind {
	for n := name; n != ""; {
		if k, ok := captureNameToKind[n]; ok {
			return k
		}
		idx := strings.LastIndex(n, ".")
		if idx < 0 {
			return KindNone
		}
		n = n[:idx]
	}
	return KindNone
}

// Highlighter manages tree-sitter parsing and highlight queries for a file.
type Highlighter struct {
	parser       *sitter.Parser
	query        *sitter.Query
	captureKinds []TokenKind // indexed by capture id
	tree         *sitter.Tree
	cache        [][]Token // per-line tokens, computed on Parse()
}

// NewHighlighter is the [editor.HighlighterFactory]-shaped constructor.
// It delegates to [New] and converts a nil concrete result into a true
// nil interface value — callers that compare `h != nil` on the returned
// [editor.Highlighter] get the answer they expect, avoiding the typed-nil
// gotcha that bites naive conversions.
func NewHighlighter(filename string) editor.Highlighter {
	h := New(filename)
	if h == nil {
		return nil
	}
	return h
}

// LanguageFor returns the tree-sitter language registered for the given
// filename's extension, or nil when the extension is unsupported.
//
// Exposed so other subsystems (e.g. the validate pipeline) can consume
// the same grammar registry without importing the full highlighter. The
// returned language is shared — callers must NOT call Close() on it.
func LanguageFor(filename string) *sitter.Language {
	ext := strings.ToLower(filepath.Ext(filename))
	spec, ok := langByExt[ext]
	if !ok {
		return nil
	}
	return spec.lang
}

// New creates a highlighter for the given file extension.
// Returns nil if the language is not supported. Setup failures (grammar
// mismatch, broken vendored query) are logged at Error level and also
// return nil so the editor falls back to unhighlighted rendering rather
// than crashing; the registry test in this package catches these before
// ship.
func New(filename string) *Highlighter {
	ext := strings.ToLower(filepath.Ext(filename))
	spec, ok := langByExt[ext]
	if !ok {
		return nil
	}

	parser := sitter.NewParser()
	if err := parser.SetLanguage(spec.lang); err != nil {
		slog.Error("highlight: SetLanguage failed", "ext", ext, "err", err)
		parser.Close()
		return nil
	}

	query, qerr := sitter.NewQuery(spec.lang, spec.scm)
	if qerr != nil {
		slog.Error("highlight: highlights.scm compile failed", "ext", ext, "err", qerr)
		parser.Close()
		return nil
	}

	names := query.CaptureNames()
	captureKinds := make([]TokenKind, len(names))
	for i, name := range names {
		captureKinds[i] = kindForCaptureName(name)
	}

	return &Highlighter{
		parser:       parser,
		query:        query,
		captureKinds: captureKinds,
	}
}

// Parse parses the source and caches highlight tokens for every line.
func (h *Highlighter) Parse(source string) {
	// Always do a full parse (nil old tree). Incremental parsing requires
	// Tree.Edit() calls describing every buffer mutation, which we don't
	// track. Without Edit() calls, tree-sitter reuses stale AST nodes
	// and produces wrong/missing tokens for modified regions.
	if h.tree != nil {
		h.tree.Close()
	}
	src := []byte(source)
	h.tree = h.parser.Parse(src, nil)
	if h.tree == nil {
		h.cache = nil
		return
	}

	lines := strings.Split(source, "\n")
	h.cache = make([][]Token, len(lines))

	qc := sitter.NewQueryCursor()
	defer qc.Close()

	captures := qc.Captures(h.query, h.tree.RootNode(), src)
	for match, idx := captures.Next(); match != nil; match, idx = captures.Next() {
		cap := match.Captures[idx]
		if int(cap.Index) >= len(h.captureKinds) {
			continue
		}
		kind := h.captureKinds[cap.Index]
		if kind == KindNone {
			continue
		}
		h.addCaptureTokens(cap.Node, kind, lines)
	}
}

// Close frees tree-sitter resources.
func (h *Highlighter) Close() {
	if h.tree != nil {
		h.tree.Close()
	}
	if h.query != nil {
		h.query.Close()
	}
	h.parser.Close()
}

// HighlightLine returns cached tokens for the given line. O(1) lookup.
func (h *Highlighter) HighlightLine(lineNum int) []Token {
	if lineNum < 0 || lineNum >= len(h.cache) {
		return nil
	}
	return h.cache[lineNum]
}

// addCaptureTokens converts a captured node's byte range into per-line
// rune-based tokens and appends them to the cache.
func (h *Highlighter) addCaptureTokens(node sitter.Node, kind TokenKind, lines []string) {
	startRow := int(node.StartPosition().Row)
	endRow := int(node.EndPosition().Row)
	startCol := int(node.StartPosition().Column)
	endCol := int(node.EndPosition().Column)

	upper := min(len(h.cache), len(lines))
	for line := startRow; line <= endRow && line < upper; line++ {
		tok, ok := byteRangeToToken(lines[line], line, startRow, endRow, startCol, endCol, kind)
		if ok {
			h.cache[line] = append(h.cache[line], tok)
		}
	}
}

// byteRangeToToken converts a node's byte-offset range on a single line to
// a rune-based Token. Returns false if the range is empty or invalid.
func byteRangeToToken(lineText string, line, startRow, endRow, startCol, endCol int, kind TokenKind) (Token, bool) {
	lineByteLen := len(lineText)

	scBytes := 0
	if line == startRow {
		scBytes = startCol
	}
	if scBytes > lineByteLen {
		scBytes = lineByteLen
	}

	ecBytes := lineByteLen
	if line == endRow {
		ecBytes = endCol
	}
	if ecBytes > lineByteLen {
		ecBytes = lineByteLen
	}

	// Defensive: tree-sitter can return stale/out-of-range positions.
	if scBytes < 0 || scBytes > lineByteLen || ecBytes < 0 || ecBytes > lineByteLen {
		return Token{}, false
	}

	sc := utf8.RuneCountInString(lineText[:scBytes])
	ec := utf8.RuneCountInString(lineText[:ecBytes])
	if sc >= ec {
		return Token{}, false
	}
	return Token{Col: sc, Len: ec - sc, Kind: kind}, true
}
