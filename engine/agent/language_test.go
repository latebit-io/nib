package agent

import "testing"

func TestDetectLanguage(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"main.go", "Go"},
		{"engine/agent/style_evaluator.go", "Go"},
		{"game.lua", "Lua"},
		{"src/level.lua", "Lua"},
		{"app.py", "Python"},
		{"types.pyi", "Python"},
		{"index.ts", "TypeScript"},
		{"App.tsx", "TypeScript"},
		{"index.js", "JavaScript"},
		{"bundle.mjs", "JavaScript"},
		{"main.rs", "Rust"},
		{"User.java", "Java"},
		{"main.kt", "Kotlin"},
		{"View.swift", "Swift"},
		{"main.c", "C"},
		{"queue.h", "C"},
		{"engine.cpp", "C++"},
		{"engine.hpp", "C++"},
		{"Program.cs", "C#"},
		{"Foo.scala", "Scala"},
		{"core.clj", "Clojure"},
		{"app.ex", "Elixir"},
		{"server.erl", "Erlang"},
		{"Main.hs", "Haskell"},
		{"parser.ml", "OCaml"},
		{"index.php", "PHP"},
		{"deploy.sh", "Shell"},
		{"setup.zsh", "Shell"},
		{"main.zig", "Zig"},
		{"util.nim", "Nim"},
		{"app.dart", "Dart"},
		{"analysis.r", "R"},
		{"plot.R", "R"},
		{"sim.jl", "Julia"},
		{"schema.sql", "SQL"},
		{"App.vue", "Vue"},
		{"App.svelte", "Svelte"},
		{"core.lisp", "Common Lisp"},
		{"interp.scm", "Scheme"},
		{"GAME.LUA", "Lua"}, // case-insensitive
		{"path/with/dirs/foo.go", "Go"},

		// Unknown / no extension — must return "" so the caller can skip
		// language guidance instead of guessing wrong.
		{"Makefile", ""},
		{"README", ""},
		{"binary.unknown", ""},
		{"", ""},
		{".gitignore", ""},
		{"some.exe", ""},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			got := DetectLanguage(tc.path)
			if got != tc.want {
				t.Errorf("DetectLanguage(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}
