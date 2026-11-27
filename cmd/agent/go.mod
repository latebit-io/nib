module github.com/latebit-io/nib/cmd/agent

go 1.27

require (
	github.com/latebit-io/nib/ai v0.10.0
	github.com/latebit-io/nib/coding v0.0.0
	github.com/latebit-io/nib/kit v0.0.0
)

require (
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/latebit-io/nib/agent v0.13.0 // indirect
	github.com/latebit-io/nib/engine v0.0.0 // indirect
	github.com/mattn/go-pointer v0.0.1 // indirect
	github.com/modelcontextprotocol/go-sdk v1.8.0 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/tree-sitter-grammars/tree-sitter-lua v0.5.0 // indirect
	github.com/tree-sitter-grammars/tree-sitter-yaml v0.7.2 // indirect
	github.com/tree-sitter/go-tree-sitter v0.25.0 // indirect
	github.com/tree-sitter/tree-sitter-go v0.25.0 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	github.com/latebit-io/nib/agent => ../../agent
	github.com/latebit-io/nib/ai => ../../ai
	github.com/latebit-io/nib/coding => ../../coding
	github.com/latebit-io/nib/engine => ../../engine
	github.com/latebit-io/nib/kit => ../../kit
)
