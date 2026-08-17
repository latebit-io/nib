package editor

import "github.com/latebit-io/nib/engine/syntax"

// The canonical definitions for [Token] and [Highlighter] live in
// [engine/syntax] so consumers of the interface do not pull in
// tree-sitter grammar blobs from [engine/highlight]. The two aliases
// below keep the controller's own signatures short; new code should
// import [engine/syntax] directly.

// Token aliases [syntax.Token].
type Token = syntax.Token

// Highlighter aliases [syntax.Highlighter].
type Highlighter = syntax.Highlighter
