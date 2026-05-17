package wire

import (
	"github.com/latebit-io/nib/engine/lint"
)

// Linters holds the auto-detected linter sets used by the agent.
//
// Linters runs at task completion (Agent.linters via NewOptions.Linters)
// and surfaces findings on edited files for the next turn to fix.
type Linters struct {
	// PostTask is the set of project-wide linters invoked after a task
	// completes. Populated from [lint.Detect] which returns golangci-lint
	// + go vet for Go projects, luacheck for Lua, etc. Nil when no
	// linter is available on PATH for the detected project type.
	PostTask []lint.Linter
}

// NewLinters returns the auto-detected linter sets for projectRoot.
// Style-driven and per-file linters were removed alongside styleconfig
// (2026-05-17 slim-down); auto-detect is the only source today.
func NewLinters(projectRoot string) Linters {
	return Linters{
		PostTask: lint.Detect(projectRoot),
	}
}
