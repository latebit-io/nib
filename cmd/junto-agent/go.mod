module github.com/latebit-io/junto/cmd/junto-agent

go 1.26

require (
	github.com/latebit-io/junto/ai v0.0.0
	github.com/latebit-io/junto/coding v0.0.0
	github.com/latebit-io/junto/engine v0.0.0
)

require (
	github.com/mattn/go-pointer v0.0.1 // indirect
	github.com/tree-sitter-grammars/tree-sitter-lua v0.5.0 // indirect
	github.com/tree-sitter-grammars/tree-sitter-yaml v0.7.2 // indirect
	github.com/tree-sitter/go-tree-sitter v0.25.0 // indirect
	github.com/tree-sitter/tree-sitter-go v0.25.0 // indirect
)

replace github.com/latebit-io/junto/engine => ../../engine

replace github.com/latebit-io/junto/ai => ../../ai

replace github.com/latebit-io/junto/coding => ../../coding
