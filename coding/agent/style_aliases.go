package agent

import (
	"time"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/style"
)

// StyleEvaluator aliases [style.StyleEvaluator] so wire/tui callers
// keep their existing identifier while the canonical type lives in
// `coding/style`.
type StyleEvaluator = style.StyleEvaluator

// StyleEvaluatorPort aliases [style.StyleEvaluatorPort] — the narrow
// interface the agent loop consumes for post-edit style review.
type StyleEvaluatorPort = style.StyleEvaluatorPort

// NewStyleEvaluator constructs a [StyleEvaluator]. Delegates to
// [style.NewStyleEvaluator].
func NewStyleEvaluator(provider llm.Provider, rules []string, timeout time.Duration) *StyleEvaluator {
	return style.NewStyleEvaluator(provider, rules, timeout)
}
