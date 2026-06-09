package loader

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	kitcmd "github.com/latebit-io/nib/kit/command"
	"github.com/latebit-io/nib/kit/frontmatter"
)

// maxCommandFileBytes caps how much of a command markdown file is read.
// A command template is small; the cap bounds memory against a hostile
// or accidental giant file in an untrusted project's .project/commands
// (a local DoS otherwise).
const maxCommandFileBytes = 1 << 20 // 1 MiB

// readCapped reads at most max bytes from the file at path, rejecting
// the file outright when it is larger. The Size() pre-check fails fast;
// the LimitReader guards against a file that grows between stat and read.
func readCapped(path string, max int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // path is a *.md under a LoadDir-walked root; see Parse.
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > max {
		return nil, fmt.Errorf("%s is %d bytes, exceeds the %d-byte command cap", path, info.Size(), max)
	}
	return io.ReadAll(io.LimitReader(f, max))
}

// commandMeta is the YAML schema parsed from the head of a markdown
// command file. All fields are optional at the YAML level; missing
// name falls back to the filename basename so a single-line file is
// a valid command.
type commandMeta struct {
	Name        string   `yaml:"name"`
	Aliases     []string `yaml:"aliases"`
	Description string   `yaml:"description"`
}

// MarkdownCommand is a [kit/command.PromptCommand] backed by an
// on-disk template. Rendering substitutes the user's argument string
// into the template body via [Substitute].
//
// Construct via [Parse] (single file) or [LoadDir] (whole directory).
// The struct is intentionally exported so consumers can introspect a
// loaded command (e.g., for diagnostics) without re-reading the file.
type MarkdownCommand struct {
	def      kitcmd.Definition
	template string
}

// Definition exposes the command's name, aliases, description, and
// source for /help and registry diagnostics.
func (m *MarkdownCommand) Definition() kitcmd.Definition { return m.def }

// Render applies argument substitution to the template body. Never
// returns an error in this implementation — substitution is total
// (missing positional args render empty, not an error). The error
// return is part of the [kit/command.PromptCommand] contract for
// future template engines (e.g. text/template) that can fail.
func (m *MarkdownCommand) Render(args string) (string, error) {
	return Substitute(m.template, args), nil
}

// Compile-time check that MarkdownCommand satisfies PromptCommand.
var _ kitcmd.PromptCommand = (*MarkdownCommand)(nil)

// Parse reads a single markdown command file and returns the
// corresponding MarkdownCommand. kind stamps Definition.Source.Kind
// so the registry's precedence model can shadow correctly.
//
// File shape:
//
//	---
//	name: review
//	description: Review the file for code quality
//	aliases: [rev]
//	---
//	Review $1 against the project's standards.
//	Focus areas: $@
//
// Frontmatter is delimited by `---` lines at the top of the file.
// Files without frontmatter are accepted: the entire body is the
// template, name comes from the filename basename, description and
// aliases default to empty.
//
// Returns an error when:
//   - the file cannot be read,
//   - frontmatter is malformed YAML,
//   - the resolved name fails Definition validation (the registry
//     will reject malformed names anyway, but Parse surfaces the
//     reason at file granularity).
func Parse(path string, kind kitcmd.SourceKind) (*MarkdownCommand, error) {
	// gosec G304: Parse is the loader's public entry point and
	// accepts a caller-provided path. The standard wiring through
	// LoadDir constrains paths to filepath.Join(root, entry.Name())
	// where root is configured at startup and entry comes from
	// os.ReadDir of that root, so attacker-controlled traversal is
	// not reachable in nib-code's call chain. External callers that
	// pass untrusted paths take on the responsibility themselves.
	// Size-capped so a hostile or accidental giant file cannot exhaust
	// memory.
	data, err := readCapped(path, maxCommandFileBytes)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var fm commandMeta
	body, err := frontmatter.Split(data, &fm)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	name := strings.TrimSpace(fm.Name)
	if name == "" {
		// Filename fallback. e.g. ".nib/commands/review.md" → "review".
		name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	name = strings.ToLower(name)

	// Use the framework's authoritative name validator. The loader
	// already lowercased name above, but ValidName accepts both
	// cases — the lowercase invariant is a loader concern, not a
	// validator concern.
	if !kitcmd.ValidName(name) {
		return nil, fmt.Errorf("parse %s: invalid command name %q (must match [a-zA-Z0-9_-]+)", path, name)
	}

	return &MarkdownCommand{
		def: kitcmd.Definition{
			Name:        name,
			Aliases:     fm.Aliases,
			Description: strings.TrimSpace(fm.Description),
			Source: kitcmd.Source{
				Kind: kind,
				Path: path,
			},
		},
		template: body,
	}, nil
}

// LoadDir parses every *.md file in root (non-recursive) and returns
// the resulting MarkdownCommands as [kit/command.Command]s ready to
// register.
//
// Error policy:
//   - Missing root: returns (nil, nil). A missing config dir is not
//     a failure; it is "no commands here." Callers that want strict
//     existence checks should stat the path themselves first.
//   - Per-file errors: each failing file is logged into the returned
//     joined error (via [errors.Join]) but does not abort the load.
//     The caller receives every successfully-parsed command AND the
//     accumulated errors so it can choose strict-or-lenient policy
//     at the composition site. Callers that want to refuse partial
//     loads can check err != nil and discard the slice.
//
// Subdirectories are NOT scanned. Future namespacing (e.g.
// ".nib/commands/review/main.md") would need explicit support.
func LoadDir(root string, kind kitcmd.SourceKind) ([]kitcmd.Command, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read commands dir %s: %w", root, err)
	}

	var cmds []kitcmd.Command
	var errs []error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if filepath.Ext(e.Name()) != ".md" {
			continue
		}
		path := filepath.Join(root, e.Name())
		cmd, perr := Parse(path, kind)
		if perr != nil {
			errs = append(errs, perr)
			continue
		}
		cmds = append(cmds, cmd)
	}
	return cmds, errors.Join(errs...)
}
