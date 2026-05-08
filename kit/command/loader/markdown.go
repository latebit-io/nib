package loader

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	kitcmd "github.com/latebit-io/nib/kit/command"
	"gopkg.in/yaml.v3"
)

// frontmatter is the YAML schema parsed from the head of a markdown
// command file. All fields are optional at the YAML level; missing
// name falls back to the filename basename so a single-line file is
// a valid command.
type frontmatter struct {
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
	data, err := os.ReadFile(path) //nolint:gosec // path is from filepath.WalkDir on a configured root
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	fm, body, err := splitFrontmatter(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	name := strings.TrimSpace(fm.Name)
	if name == "" {
		// Filename fallback. e.g. ".nib/commands/review.md" → "review".
		name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	name = strings.ToLower(name)

	if !validCommandName(name) {
		return nil, fmt.Errorf("parse %s: invalid command name %q (must match [a-z0-9_-]+)", path, name)
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

// splitFrontmatter extracts a YAML frontmatter block (if present)
// from a markdown file's bytes and returns the parsed metadata plus
// the remaining body. Files without a frontmatter block are valid:
// the whole content becomes the body and the metadata is the zero
// value.
//
// Frontmatter is detected by a leading `---` line; the closing `---`
// must appear on its own line. Anything else (e.g. an in-body `---`
// horizontal rule on a file with no frontmatter) is left untouched
// in the body.
func splitFrontmatter(data []byte) (frontmatter, string, error) {
	const fence = "---"
	// Strip a UTF-8 BOM if present so the fence detector lines up
	// with the user's intended first line. Editors on Windows
	// sometimes inject one; tolerating it costs nothing.
	const utf8BOM = "\xef\xbb\xbf"
	text := strings.TrimPrefix(string(data), utf8BOM)

	if !strings.HasPrefix(text, fence) {
		return frontmatter{}, text, nil
	}
	// First line must be exactly "---" (allow trailing whitespace
	// but no other content). A `---` with content on the same line
	// is treated as body content, matching the YAML frontmatter
	// convention used by static site generators.
	rest := text[len(fence):]
	nl := strings.IndexByte(rest, '\n')
	if nl < 0 || strings.TrimSpace(rest[:nl]) != "" {
		return frontmatter{}, text, nil
	}
	rest = rest[nl+1:]

	// Find the closing fence on its own line.
	var fmText string
	for {
		eol := strings.IndexByte(rest, '\n')
		var line string
		if eol < 0 {
			line = rest
		} else {
			line = rest[:eol]
		}
		if strings.TrimSpace(line) == fence {
			body := ""
			if eol >= 0 {
				body = rest[eol+1:]
			}
			var fm frontmatter
			if strings.TrimSpace(fmText) != "" {
				if err := yaml.Unmarshal([]byte(fmText), &fm); err != nil {
					return frontmatter{}, "", fmt.Errorf("frontmatter: %w", err)
				}
			}
			return fm, body, nil
		}
		fmText += line + "\n"
		if eol < 0 {
			// EOF without closing fence — treat the whole file as
			// body, matching the lenient policy ("missing close
			// means no frontmatter").
			return frontmatter{}, text, nil
		}
		rest = rest[eol+1:]
	}
}

// validCommandName mirrors the registry's name validator so Parse
// can fail at file granularity rather than deferring the error to
// Register (which would only know "command X failed", not "file Y
// produced a bad name"). Kept in sync with kit/command/parse.go's
// rule by convention; the registry's own validator is the
// authoritative gate.
func validCommandName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}
