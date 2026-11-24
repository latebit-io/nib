module github.com/latebit-io/nib/kit

go 1.27

// Sibling requires name real release tags: replace is not inherited, so
// an untagged version here breaks `go get .../nib/kit` for consumers.
// release.yml tags ai, agent, kit together; bump both when kit needs more.
require (
	github.com/latebit-io/nib/agent v0.3.0
	github.com/latebit-io/nib/ai v0.3.0
	gopkg.in/yaml.v3 v3.0.1
)

replace (
	github.com/latebit-io/nib/agent => ../agent
	github.com/latebit-io/nib/ai => ../ai
)
