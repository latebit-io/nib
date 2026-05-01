module github.com/latebit-io/nib/coding

go 1.26

require (
	github.com/latebit-io/nib/agent v0.0.0
	github.com/latebit-io/nib/ai v0.0.0
	github.com/latebit-io/nib/engine v0.0.0
)

require (
	github.com/mattn/go-pointer v0.0.1 // indirect
	github.com/tree-sitter/go-tree-sitter v0.25.0 // indirect
)

replace github.com/latebit-io/nib/agent => ../agent

replace github.com/latebit-io/nib/ai => ../ai

replace github.com/latebit-io/nib/engine => ../engine
