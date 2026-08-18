package highlight

import (
	"strings"
	"testing"

	"github.com/latebit-io/nib/engine/contracttest"
	"github.com/latebit-io/nib/engine/syntax"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// collectKinds flattens every line's tokens into the set of TokenKinds seen.
func collectKinds(h *Highlighter, lineCount int) map[syntax.TokenKind]int {
	counts := map[syntax.TokenKind]int{}
	for i := range lineCount {
		for _, tok := range h.HighlightLine(i) {
			counts[tok.Kind]++
		}
	}
	return counts
}

func TestNew_UnsupportedExtensionReturnsNil(t *testing.T) {
	if h := New("foo.xyz"); h != nil {
		t.Fatalf("expected nil for unsupported extension, got %v", h)
	}
}

func TestNew_SupportedExtensions(t *testing.T) {
	cases := []string{"foo.go", "foo.lua", "FOO.LUA", "foo.yaml", "foo.yml", "FOO.YML"}
	for _, name := range cases {
		h := New(name)
		if h == nil {
			t.Errorf("New(%q) returned nil", name)
			continue
		}
		h.Close()
	}
}

// TestRegistry_AllLanguagesCompile ensures every entry in langByExt has a
// grammar that accepts its vendored highlights.scm. Catches broken queries
// and grammar/binding version mismatches at CI time rather than silently
// disabling highlighting at runtime.
func TestRegistry_AllLanguagesCompile(t *testing.T) {
	for ext, spec := range langByExt {
		parser := sitter.NewParser()
		if err := parser.SetLanguage(spec.lang); err != nil {
			parser.Close()
			t.Errorf("%s: SetLanguage failed: %v", ext, err)
			continue
		}
		query, err := sitter.NewQuery(spec.lang, spec.scm)
		if err != nil {
			parser.Close()
			t.Errorf("%s: highlights.scm compile failed: %v", ext, err)
			continue
		}
		query.Close()
		parser.Close()
	}
}

func TestParse_EmptySourceDoesNotPanic(t *testing.T) {
	h := New("empty.go")
	if h == nil {
		t.Fatal("expected highlighter for .go")
	}
	defer h.Close()
	h.Parse("")
	if got := h.HighlightLine(0); got != nil {
		t.Errorf("expected nil tokens for empty source, got %v", got)
	}
}

func TestParse_GoProducesExpectedKinds(t *testing.T) {
	src := `package main

import "fmt"

// Greet prints a greeting.
func Greet(name string) {
	fmt.Println("hello", name, 42)
}
`
	h := New("main.go")
	if h == nil {
		t.Fatal("expected highlighter for .go")
	}
	defer h.Close()
	h.Parse(src)

	lineCount := strings.Count(src, "\n") + 1
	kinds := collectKinds(h, lineCount)

	for _, want := range []syntax.TokenKind{syntax.KindKeyword, syntax.KindString, syntax.KindComment, syntax.KindNumber, syntax.KindFunction} {
		if kinds[want] == 0 {
			t.Errorf("expected at least one %s token in Go source, got kinds=%v", want, kinds)
		}
	}
}

func TestParse_LuaProducesExpectedKinds(t *testing.T) {
	src := `-- simple LÖVE-style module
local M = {}

function M.greet(name)
	print("hello " .. name, 42)
	return true
end

return M
`
	h := New("mod.lua")
	if h == nil {
		t.Fatal("expected highlighter for .lua")
	}
	defer h.Close()
	h.Parse(src)

	lineCount := strings.Count(src, "\n") + 1
	kinds := collectKinds(h, lineCount)

	for _, want := range []syntax.TokenKind{syntax.KindKeyword, syntax.KindString, syntax.KindComment, syntax.KindNumber, syntax.KindFunction, syntax.KindConstant} {
		if kinds[want] == 0 {
			t.Errorf("expected at least one %s token in Lua source, got kinds=%v", want, kinds)
		}
	}
}

func TestParse_YAMLProducesExpectedKinds(t *testing.T) {
	src := `---
project: TestProject
enabled: true
count: 42
name: "TestProject"
tags:
  - ai
  - editor
anchors: &base
  foo: bar
alias: *base
`
	h := New("config.yaml")
	if h == nil {
		t.Fatal("expected highlighter for .yaml")
	}
	defer h.Close()
	h.Parse(src)

	lineCount := strings.Count(src, "\n") + 1
	kinds := collectKinds(h, lineCount)

	for _, want := range []syntax.TokenKind{syntax.KindKeyword, syntax.KindString, syntax.KindNumber, syntax.KindConstant, syntax.KindProperty} {
		if kinds[want] == 0 {
			t.Errorf("expected at least one %s token in YAML source, got kinds=%v", want, kinds)
		}
	}
}

func TestKindForCaptureName(t *testing.T) {
	cases := []struct {
		name string
		want syntax.TokenKind
	}{
		{"keyword", syntax.KindKeyword},
		{"keyword.function", syntax.KindKeyword},
		{"keyword.return", syntax.KindKeyword},
		{"string", syntax.KindString},
		{"string.escape", syntax.KindString},
		{"function", syntax.KindFunction},
		{"function.call", syntax.KindFunction},
		{"function.builtin", syntax.KindFunction},
		{"method.call", syntax.KindFunction},
		{"constant.builtin", syntax.KindConstant},
		{"boolean", syntax.KindConstant},
		{"variable", syntax.KindNone},
		{"variable.builtin", syntax.KindNone},
		{"punctuation.bracket", syntax.KindNone},
		{"totally_made_up", syntax.KindNone},
		{"", syntax.KindNone},
	}
	for _, tc := range cases {
		if got := kindForCaptureName(tc.name); got != tc.want {
			t.Errorf("kindForCaptureName(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestHighlighter_SatisfiesContract verifies the tree-sitter Go
// highlighter conforms to the [syntax.Highlighter] contract: arbitrary
// source parsing, out-of-range line lookup, repeated Parse, Close
// safety. The fixture uses a `.go` filename so the registry returns a
// concrete (non-nil) highlighter.
func TestHighlighter_SatisfiesContract(t *testing.T) {
	contracttest.Highlighter(t, func() syntax.Highlighter {
		h := NewHighlighter("contract.go")
		if h == nil {
			t.Fatal("NewHighlighter(\"contract.go\") returned nil — Go grammar registry broken")
		}
		return h
	})
}
