// Package highlight provides syntax highlighting via tree-sitter.
package highlight

import (
	"log/slog"
	"path/filepath"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_go "github.com/tree-sitter/tree-sitter-go/bindings/go"
)

// TokenKind identifies the syntactic role of a token.
// Frontends map these to their own styling (lipgloss, CSS, etc.).
type TokenKind string

const (
	KindKeyword  TokenKind = "keyword"
	KindString   TokenKind = "string"
	KindComment  TokenKind = "comment"
	KindNumber   TokenKind = "number"
	KindType     TokenKind = "type"
	KindOperator TokenKind = "operator"
	KindNone     TokenKind = ""
)

// Token represents a highlighted range on a single line.
type Token struct {
	Col  int
	Len  int
	Kind TokenKind
}

// Highlighter manages tree-sitter parsing and highlight queries for a file.
type Highlighter struct {
	parser *sitter.Parser
	tree   *sitter.Tree
	lang   *sitter.Language
	cache  [][]Token // per-line tokens, computed on Parse()
}

// New creates a highlighter for the given file extension.
// Returns nil if the language is not supported.
func New(filename string) *Highlighter {
	ext := strings.ToLower(filepath.Ext(filename))
	lang := languageForExt(ext)
	if lang == nil {
		return nil
	}

	parser := sitter.NewParser()
	if err := parser.SetLanguage(lang); err != nil {
		return nil
	}

	return &Highlighter{
		parser: parser,
		lang:   lang,
	}
}

// Parse parses the source and caches highlight tokens for every line.
func (h *Highlighter) Parse(source string) {
	h.tree = h.parser.Parse([]byte(source), h.tree)
	if h.tree == nil {
		h.cache = nil
		return
	}

	lines := strings.Split(source, "\n")

	h.cache = make([][]Token, len(lines))
	h.collectAllTokens(h.tree.RootNode(), lines)
}

// Close frees tree-sitter resources.
func (h *Highlighter) Close() {
	if h.tree != nil {
		h.tree.Close()
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

// collectAllTokens walks the tree once and populates h.cache for every line.
// Tree-sitter uses byte offsets; we convert to rune indices for the editor.
func (h *Highlighter) collectAllTokens(node *sitter.Node, lines []string) {
	childCount := node.ChildCount()
	if childCount == 0 {
		startRow := int(node.StartPosition().Row)
		endRow := int(node.EndPosition().Row)
		startCol := int(node.StartPosition().Column)
		endCol := int(node.EndPosition().Column)

		kind := kindForNode(node.GrammarName())
		if kind == KindNone {
			return
		}

		for line := startRow; line <= endRow && line < len(h.cache); line++ {
			if line >= len(lines) {
				slog.Debug("highlight: line index out of range", "line", line, "len", len(lines))
				continue
			}
			lineBytes := lines[line]
			lineByteLen := len(lineBytes)

			// Byte offsets for this line, clamped to line length
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

			// Defensive: ensure byte offsets are valid slice bounds.
			// Tree-sitter incremental parsing can return stale positions
			// when the source changes significantly between parses.
			if scBytes < 0 || scBytes > lineByteLen || ecBytes < 0 || ecBytes > lineByteLen {
				continue
			}

			// Convert byte offsets to rune offsets
			sc := len([]rune(lineBytes[:scBytes]))
			ec := len([]rune(lineBytes[:ecBytes]))

			if sc < ec {
				h.cache[line] = append(h.cache[line], Token{
					Col:  sc,
					Len:  ec - sc,
					Kind: kind,
				})
			}
		}
		return
	}

	for i := range childCount {
		h.collectAllTokens(node.Child(i), lines)
	}
}

func kindForNode(nodeType string) TokenKind {
	switch nodeType {
	// Keywords
	case "func", "return", "if", "else", "for", "range", "switch", "case",
		"default", "break", "continue", "goto", "fallthrough",
		"var", "const", "type", "struct", "interface", "map",
		"chan", "go", "defer", "select", "package", "import",
		"true", "false", "nil":
		return KindKeyword

	// Strings
	case "interpreted_string_literal", "raw_string_literal", "rune_literal",
		"string", "escape_sequence":
		return KindString

	// Comments
	case "comment", "line_comment", "block_comment":
		return KindComment

	// Numbers
	case "int_literal", "float_literal", "imaginary_literal",
		"integer", "float":
		return KindNumber

	// Types
	case "type_identifier", "field_identifier":
		return KindType

	// Operators
	case ":=", "=", "==", "!=", "<", ">", "<=", ">=",
		"+", "-", "*", "/", "%", "&", "|", "^",
		"&&", "||", "!", "++", "--", "<<", ">>":
		return KindOperator
	}

	return KindNone
}

func languageForExt(ext string) *sitter.Language {
	switch ext {
	case ".go":
		return sitter.NewLanguage(tree_sitter_go.Language())
	}
	return nil
}
