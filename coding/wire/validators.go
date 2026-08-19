package wire

import (
	"os"

	"github.com/latebit-io/nib/ai/brand"
	"github.com/latebit-io/nib/engine/highlight"
	"github.com/latebit-io/nib/engine/validate"
	"github.com/latebit-io/nib/engine/validate/goparse"
	"github.com/latebit-io/nib/engine/validate/treesitter"
)

// NewValidationPipeline returns the standard pre-approval validator
// pipeline (Go parse + tree-sitter syntax) shared by the TUI and
// headless binaries. Headless / CI mode wires the same stages: with no
// developer watching, a parser-breaking edit would otherwise land
// unnoticed. Returns nil (validation disabled) when the
// brand VALIDATORS_DISABLED env var is set non-empty.
func NewValidationPipeline() validate.Pipeline {
	if os.Getenv(brand.EnvKeyValidatorsDisabled) != "" {
		return nil
	}
	return validate.NewPipeline(
		goparse.Validator{},
		treesitter.New(highlight.LanguageFor),
	)
}
