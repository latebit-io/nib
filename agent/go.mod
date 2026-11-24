module github.com/latebit-io/nib/agent

go 1.27

// The sibling require names a real release tag: replace is not inherited,
// so an untagged version breaks `go get .../nib/agent` for consumers.
// release.yml tags ai, agent, kit together; bump it when agent needs more.
require github.com/latebit-io/nib/ai v0.3.0

// Monorepo development builds against the sibling checkout; consumers never
// see this line and resolve the tagged require above.
replace github.com/latebit-io/nib/ai => ../ai
