package editor

import "github.com/latebit-io/nib/engine/syntax"

// The canonical definitions for [TokenKind], [Token], [Highlighter], and
// [HighlighterFactory] live in [engine/syntax] so that consumers of the
// interface (the editor controller, future TUI relocation) do not pull
// in tree-sitter grammar blobs from [engine/highlight].
//
// The aliases below preserve the existing `editor.Token`, `editor.Highlighter`
// surface so callers outside this package keep compiling. New code should
// import [engine/syntax] directly.

// TokenKind aliases [syntax.TokenKind].
type TokenKind = syntax.TokenKind

// Token aliases [syntax.Token].
type Token = syntax.Token

// Highlighter aliases [syntax.Highlighter].
type Highlighter = syntax.Highlighter

// HighlighterFactory aliases [syntax.HighlighterFactory].
type HighlighterFactory = syntax.HighlighterFactory

// Token-kind constants re-exported from [engine/syntax].
const (
	KindKeyword  = syntax.KindKeyword
	KindString   = syntax.KindString
	KindComment  = syntax.KindComment
	KindNumber   = syntax.KindNumber
	KindType     = syntax.KindType
	KindProperty = syntax.KindProperty
	KindOperator = syntax.KindOperator
	KindFunction = syntax.KindFunction
	KindConstant = syntax.KindConstant
	KindNone     = syntax.KindNone
)
