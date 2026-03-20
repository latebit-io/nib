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

// Parse parses (or re-parses) the given source code.
func (h *Highlighter) Parse(source string) {
	h.tree = h.parser.Parse([]byte(source), h.tree)
}

// Close frees tree-sitter resources.
func (h *Highlighter) Close() {
	if h.tree != nil {
		h.tree.Close()
	}
	h.parser.Close()
}

// HighlightLine returns styled tokens for the given line of source.
// lineNum is 0-indexed. source is the full file content.
func (h *Highlighter) HighlightLine(lineNum int, source string) []Token {
	if h.tree == nil {
		return nil
	}

	root := h.tree.RootNode()
	return h.collectTokens(root, lineNum, []byte(source))
}

func (h *Highlighter) collectTokens(node *sitter.Node, lineNum int, source []byte) []Token {
	var tokens []Token

	childCount := node.ChildCount()
	if childCount == 0 {
		// Leaf node — check if it's on our line
		startRow := int(node.StartPosition().Row)
		endRow := int(node.EndPosition().Row)
		if lineNum < startRow || lineNum > endRow {
			return nil
		}

		startCol := int(node.StartPosition().Column)
		endCol := int(node.EndPosition().Column)

		// Clamp to this line
		if lineNum > startRow {
			startCol = 0
		}
		if lineNum < endRow {
			// Node spans multiple lines; this line goes to end
			// Use the line length as endCol
			lines := strings.Split(string(source), "\n")
			if lineNum < len(lines) {
				endCol = len(lines[lineNum])
			}
		}

		nodeType := node.GrammarName()
		style := styleForNode(nodeType)
		if startCol < endCol {
			tokens = append(tokens, Token{
				Col:   startCol,
				Len:   endCol - startCol,
				Style: style,
			})
		}
		return tokens
	}

	for i := range childCount {
		child := node.Child(i)
		childTokens := h.collectTokens(child, lineNum, source)
		tokens = append(tokens, childTokens...)
	}

	return tokens
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
