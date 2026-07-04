module github.com/latebit-io/nib/agent

go 1.26

// A real, tagged version so external consumers can resolve this module —
// dependency `replace` directives are not inherited, so a v0.0.0 require
// made `go get .../nib/agent` fail outside this repo. Release discipline:
// when cutting agent/vX.Y.Z, tag ai/vX.Y.Z at (or before) the same commit
// and point this require at it.
require github.com/latebit-io/nib/ai v0.1.0

// Monorepo development builds against the sibling checkout; consumers never
// see this line and resolve the tagged require above.
replace github.com/latebit-io/nib/ai => ../ai
