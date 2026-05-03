module github.com/latebit-io/nib/engine

go 1.26

require (
	github.com/latebit-io/nib/ai v0.0.0
	github.com/latebit-io/nib/kit v0.0.0
	github.com/tree-sitter-grammars/tree-sitter-lua v0.5.0
	github.com/tree-sitter-grammars/tree-sitter-yaml v0.7.2
	github.com/tree-sitter/go-tree-sitter v0.25.0
	github.com/tree-sitter/tree-sitter-go v0.25.0
)

replace (
	github.com/latebit-io/nib/agent => ../agent
	github.com/latebit-io/nib/ai => ../ai
	github.com/latebit-io/nib/kit => ../kit
)

require github.com/mattn/go-pointer v0.0.1 // indirect
