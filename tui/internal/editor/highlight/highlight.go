// Package highlight provides syntax highlighting via tree-sitter.
package highlight

import (
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
	sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_go "github.com/tree-sitter/tree-sitter-go/bindings/go"
)

// Token represents a highlighted range on a single line.
type Token struct {
	Col   int
	Len   int
	Style lipgloss.Style
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
	lineLens := make([]int, len(lines))
	for i, l := range lines {
		lineLens[i] = len(l)
	}

	h.cache = make([][]Token, len(lines))
	h.collectAllTokens(h.tree.RootNode(), lineLens)
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
func (h *Highlighter) collectAllTokens(node *sitter.Node, lineLens []int) {
	childCount := node.ChildCount()
	if childCount == 0 {
		startRow := int(node.StartPosition().Row)
		endRow := int(node.EndPosition().Row)
		startCol := int(node.StartPosition().Column)
		endCol := int(node.EndPosition().Column)

		style := styleForNode(node.GrammarName())
		if style.GetForeground() == nil {
			return
		}

		for line := startRow; line <= endRow && line < len(h.cache); line++ {
			sc := 0
			if line == startRow {
				sc = startCol
			}
			ec := lineLens[line]
			if line == endRow {
				ec = endCol
			}
			if sc < ec {
				h.cache[line] = append(h.cache[line], Token{
					Col:   sc,
					Len:   ec - sc,
					Style: style,
				})
			}
		}
		return
	}

	for i := range childCount {
		h.collectAllTokens(node.Child(i), lineLens)
	}
}

// Theme colors
var (
	keywordStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("5")) // magenta
	stringStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // green
	commentStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("8")) // gray
	numberStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("3")) // yellow
	typeStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("6")) // cyan
	funcStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("4")) // blue
	operatorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("9")) // bright red
	defaultStyle  = lipgloss.NewStyle()
)

func styleForNode(nodeType string) lipgloss.Style {
	switch nodeType {
	// Keywords
	case "func", "return", "if", "else", "for", "range", "switch", "case",
		"default", "break", "continue", "goto", "fallthrough",
		"var", "const", "type", "struct", "interface", "map",
		"chan", "go", "defer", "select", "package", "import",
		"true", "false", "nil":
		return keywordStyle

	// Strings
	case "interpreted_string_literal", "raw_string_literal", "rune_literal",
		"string", "escape_sequence":
		return stringStyle

	// Comments
	case "comment", "line_comment", "block_comment":
		return commentStyle

	// Numbers
	case "int_literal", "float_literal", "imaginary_literal",
		"integer", "float":
		return numberStyle

	// Types
	case "type_identifier", "field_identifier":
		return typeStyle

	// Functions
	case "function_declaration", "method_declaration":
		return funcStyle

	// Operators
	case ":=", "=", "==", "!=", "<", ">", "<=", ">=",
		"+", "-", "*", "/", "%", "&", "|", "^",
		"&&", "||", "!", "++", "--", "<<", ">>":
		return operatorStyle
	}

	return defaultStyle
}

func languageForExt(ext string) *sitter.Language {
	switch ext {
	case ".go":
		return sitter.NewLanguage(tree_sitter_go.Language())
	}
	return nil
}
