module github.com/latebit-io/junto/coding

go 1.26

require (
	github.com/latebit-io/junto/ai v0.0.0
	github.com/latebit-io/junto/engine v0.0.0
)

require (
	github.com/mattn/go-pointer v0.0.1 // indirect
	github.com/tree-sitter/go-tree-sitter v0.25.0 // indirect
)

replace github.com/latebit-io/junto/ai => ../ai

replace github.com/latebit-io/junto/engine => ../engine
