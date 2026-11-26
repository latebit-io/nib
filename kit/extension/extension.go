// Package extension defines what turns a base agent into a particular
// one: a named bundle of tools, hooks, prompt text and cleanup, built
// per session by a factory a binary registers.
package extension

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/permission"
	"github.com/latebit-io/nib/kit/toolperm"
)

// Extension is one switched-on unit of a session, as its factory
// built it. The name and description live on its [Spec].
type Extension struct {
	// Toolset is its tools, hooks and slash commands.
	Toolset kit.Toolset
	// Prompt is added to the system prompt. Empty adds nothing.
	Prompt string
	// Close releases what the extension holds. Nil when it holds nothing.
	Close func() error
}

// Env is what a session gives a factory.
type Env struct {
	// Dir is the session's working directory.
	Dir string
	// Root is the project root Dir belongs to.
	Root string
	// Permission is the session's gate. An extension registers a policy
	// for each of its tools, saying what to show the person or that the
	// tool only reads. Nil means the session has no gate.
	Permission *permission.Gate
}

// Factory builds an extension for one session.
type Factory func(ctx context.Context, env Env) (Extension, error)

// Spec is a compiled extension a binary offers.
type Spec struct {
	// Name is what configuration, flags and commands call it.
	Name string
	// Description is shown when extensions are listed.
	Description string
	// Default switches the extension on unless a session turns it off.
	Default bool
	// New builds it for a session.
	New Factory
}

// Registry is the set of compiled extensions a binary offers.
type Registry struct {
	specs []Spec
}

// NewRegistry returns a registry of specs, in the order given. A spec
// without a name or factory, or a repeated name, is an error.
func NewRegistry(specs ...Spec) (*Registry, error) {
	seen := map[string]bool{}
	for _, s := range specs {
		if s.Name == "" || s.New == nil {
			return nil, fmt.Errorf("extension: spec %q needs a name and a factory", s.Name)
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("extension: %q is registered twice", s.Name)
		}
		seen[s.Name] = true
	}
	return &Registry{specs: slices.Clone(specs)}, nil
}

// Specs returns every registered extension, in registration order.
// Safe on a nil Registry, which offers none.
func (r *Registry) Specs() []Spec {
	if r == nil {
		return nil
	}
	return slices.Clone(r.specs)
}

// Select returns the extensions a session runs: the defaults and those
// s enables, without those it disables, which wins. A name the registry
// does not know is an error: a typo must not silently switch nothing.
func (r *Registry) Select(s Switches) ([]Spec, error) {
	specs := r.Specs()
	var errs []error
	for _, name := range slices.Concat(s.Enable, s.Disable) {
		if !slices.ContainsFunc(specs, func(spec Spec) bool { return spec.Name == name }) {
			errs = append(errs, fmt.Errorf("extension: unknown extension %q", name))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	var on []Spec
	for _, spec := range specs {
		if (spec.Default || slices.Contains(s.Enable, spec.Name)) && !slices.Contains(s.Disable, spec.Name) {
			on = append(on, spec)
		}
	}
	return on, nil
}

// Restrict returns e with only the tools m allows: with an allow list,
// the tools it grants; in either case none it denies outright. This is
// per-extension tool filtering, on the same grants skills use.
func Restrict(e Extension, m *toolperm.Matcher) Extension {
	if m == nil {
		return e
	}
	e.Toolset.Tools = slices.DeleteFunc(slices.Clone(e.Toolset.Tools), func(t kit.Tool) bool {
		name := t.Definition().Function.Name
		return m.DeniesTool(name) || (m.HasAllowList() && !m.GrantsTool(name))
	})
	return e
}
