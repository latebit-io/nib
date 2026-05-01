package agent

import "github.com/latebit-io/nib/coding/budget"

// SessionUsage is an alias for [budget.Session] preserving the
// previous in-package surface (the type that [Agent.Usage] returns)
// while phase 7 sorts out the canonical-package renames. New code in
// other packages should refer to [budget.Session] directly.
type SessionUsage = budget.Session
