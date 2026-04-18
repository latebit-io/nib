package highlight

import (
	"strings"
	"testing"
)

// collectKinds flattens every line's tokens into the set of TokenKinds seen.
func collectKinds(h *Highlighter, lineCount int) map[TokenKind]int {
	counts := map[TokenKind]int{}
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
	cases := []string{"foo.go", "foo.lua", "FOO.LUA"}
	for _, name := range cases {
		h := New(name)
		if h == nil {
			t.Errorf("New(%q) returned nil", name)
			continue
		}
		h.Close()
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

	for _, want := range []TokenKind{KindKeyword, KindString, KindComment, KindNumber, KindFunction} {
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

	for _, want := range []TokenKind{KindKeyword, KindString, KindComment, KindNumber, KindFunction, KindConstant} {
		if kinds[want] == 0 {
			t.Errorf("expected at least one %s token in Lua source, got kinds=%v", want, kinds)
		}
	}
}

func TestKindForCaptureName(t *testing.T) {
	cases := []struct {
		name string
		want TokenKind
	}{
		{"keyword", KindKeyword},
		{"keyword.function", KindKeyword},
		{"keyword.return", KindKeyword},
		{"string", KindString},
		{"string.escape", KindString},
		{"function", KindFunction},
		{"function.call", KindFunction},
		{"function.builtin", KindFunction},
		{"method.call", KindFunction},
		{"constant.builtin", KindConstant},
		{"boolean", KindConstant},
		{"variable", KindNone},
		{"variable.builtin", KindNone},
		{"punctuation.bracket", KindNone},
		{"totally_made_up", KindNone},
		{"", KindNone},
	}
	for _, tc := range cases {
		if got := kindForCaptureName(tc.name); got != tc.want {
			t.Errorf("kindForCaptureName(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}
