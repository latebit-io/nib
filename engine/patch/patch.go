// Package patch parses Codex-style patch envelopes into a typed
// representation that the agent's apply_patch tool can resolve against
// real file content.
//
// The format is a subset of the Codex / Aider / OpenAI patch envelope:
//
//	*** Begin Patch
//	*** Update File: path/to/file.ext
//	@@ optional context anchor
//	-old line
//	+new line
//	 unchanged context line
//	*** End Patch
//
// v1 scope:
//
//   - One *** Update File section per envelope. Multiple hunks
//     (multiple @@ blocks) within that file are allowed.
//   - @@ anchors are optional. When present, they are used at apply
//     time to disambiguate hunks whose -/+ lines could match in more
//     than one place.
//   - Context lines (leading space, or a bare line with no -/+/@@/***
//     prefix) must match the surrounding file content verbatim. The
//     leading space is stripped during parse.
//
// Intentionally NOT in v1: *** Add File (use write_file), *** Delete
// File (needs its own approval shape), multi-file envelopes (require
// multi-proposal approval), git-format diffs.
//
// The parser is stdlib-only and produces typed errors so the tool
// layer can surface specific guidance back to the LLM — e.g. "use
// write_file for new files" when an *** Add File directive arrives.
package patch

import (
	"errors"
	"fmt"
	"strings"
)

// HunkKind classifies a line inside a hunk: a deletion, an insertion,
// or an unchanged context line.
type HunkKind int

const (
	// HunkContext is an unchanged surrounding line. Context lines must
	// match the file verbatim at apply time; they exist to anchor the
	// hunk and to disambiguate when the same -/+ pair appears more than
	// once in the file.
	HunkContext HunkKind = iota
	// HunkDelete is a `-` line: text that exists in the file and must
	// be removed.
	HunkDelete
	// HunkInsert is a `+` line: text that does not exist in the file
	// and must be added in the deleted lines' place.
	HunkInsert
)

// HunkLine is one line inside a hunk. Text is the line content with
// the leading `-`/`+`/` ` prefix stripped; the newline separator is
// not included.
type HunkLine struct {
	Kind HunkKind
	Text string
}

// Hunk is one contiguous edit within a file. Anchor is the substring
// from the `@@ ...` line (empty when the @@ marker was omitted); at
// apply time the anchor narrows the search window to portions of the
// file that follow a line containing this substring. Lines preserves
// the original ordering of context/delete/insert lines so the apply
// step can reconstruct the post-edit content.
type Hunk struct {
	Anchor string
	Lines  []HunkLine
}

// FilePatch collects every hunk targeting a single file path.
type FilePatch struct {
	Path  string
	Hunks []Hunk
}

// Patch is the parsed envelope. In v1 Files always has length 1 on
// successful parse; the slice shape is preserved so v2 can add
// multi-file patches without breaking the type.
type Patch struct {
	Files []FilePatch
}

// Sentinel errors returned (wrapped in [ParseError]) for each
// well-defined failure mode. Callers use [errors.Is] to branch on
// them — e.g. the apply_patch tool turns [ErrUnsupportedV2] into a
// targeted "use write_file" message back to the LLM.
var (
	// ErrMissingBegin signals the envelope did not start with
	// `*** Begin Patch` (after any leading blank lines were trimmed).
	ErrMissingBegin = errors.New("missing '*** Begin Patch' header")
	// ErrMissingEnd signals the envelope ran out of input before
	// `*** End Patch` arrived.
	ErrMissingEnd = errors.New("missing '*** End Patch' terminator")
	// ErrUnknownDirective signals a `*** ...` line that is not one of
	// the recognised directives (Begin / End / Update File / Add File
	// / Delete File).
	ErrUnknownDirective = errors.New("unknown '*** ...' directive")
	// ErrEmptyHunk signals a hunk that contained only context lines
	// (no `-` and no `+`) — there is nothing to apply.
	ErrEmptyHunk = errors.New("hunk has no -/+ lines")
	// ErrNoFile signals hunk content arrived before any
	// `*** Update File` directive named the target.
	ErrNoFile = errors.New("hunk content before '*** Update File' directive")
	// ErrMultiFile signals a second `*** Update File` directive
	// appeared in the same envelope. v1 supports one file per call;
	// v2 may lift this once multi-proposal approval exists.
	ErrMultiFile = errors.New("v1 supports only one '*** Update File' per patch — split into separate apply_patch calls")
	// ErrMissingPath signals an `*** Update File:` directive with no
	// path after the colon.
	ErrMissingPath = errors.New("'*** Update File:' requires a path")
)

// UnsupportedV2Error is returned (wrapped in [ParseError]) for
// directives the format reserves but v1 deliberately does not
// implement: `*** Add File` and `*** Delete File`. The Directive
// field names which one so the tool layer can steer the LLM toward
// the right replacement (`write_file` for Add; no replacement yet
// for Delete).
type UnsupportedV2Error struct {
	Directive string
}

// Error implements the error interface.
func (e *UnsupportedV2Error) Error() string {
	switch e.Directive {
	case "Add File":
		return "'*** Add File' is not supported in v1 — use write_file to create new files"
	case "Delete File":
		return "'*** Delete File' is not supported in v1"
	default:
		return fmt.Sprintf("'*** %s' is not supported in v1", e.Directive)
	}
}

// Is reports whether target is also an [UnsupportedV2Error]. The
// directive name does not have to match — callers branch on the type,
// then read [UnsupportedV2Error.Directive] for specifics.
func (e *UnsupportedV2Error) Is(target error) bool {
	_, ok := target.(*UnsupportedV2Error)
	return ok
}

// ParseError pins a parse failure to a specific line so the LLM gets
// actionable feedback (and a human reviewer can find the problem
// quickly). Err is the underlying sentinel (one of the package-level
// vars or an [UnsupportedV2Error]); [ParseError.Unwrap] returns it so
// [errors.Is] / [errors.As] compose naturally.
type ParseError struct {
	// Line is 1-indexed; 0 means the error is not tied to a specific
	// line (e.g. completely empty input).
	Line int
	// Msg is the human-readable explanation; it embeds the sentinel
	// message plus any extra context (the offending line text, the
	// expected shape, etc.).
	Msg string
	// Err is the wrapped sentinel error.
	Err error
}

// Error implements the error interface.
func (e *ParseError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("patch: line %d: %s", e.Line, e.Msg)
	}
	return fmt.Sprintf("patch: %s", e.Msg)
}

// Unwrap returns the wrapped sentinel so [errors.Is] and [errors.As]
// can branch on the underlying error kind.
func (e *ParseError) Unwrap() error { return e.Err }

// Parse converts a raw patch envelope into a [Patch]. The input may
// use either Unix or DOS line endings; the parser normalises to LF
// internally and the original separator is not preserved. Leading and
// trailing blank lines are tolerated; any other content outside the
// `*** Begin Patch` / `*** End Patch` envelope returns an error.
func Parse(raw string) (*Patch, error) {
	lines := splitLines(raw)
	p := &parser{lines: lines}
	return p.run()
}

// splitLines normalises line endings and strips one trailing empty
// element produced by a final LF (so a patch ending in "\n*** End
// Patch\n" doesn't carry a phantom blank line). Leading blank lines
// are kept in the slice but advanced past by [parser.run] before the
// `*** Begin Patch` check; that way line numbers in errors still
// correspond to the raw input.
func splitLines(raw string) []string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	lines := strings.Split(raw, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// parser holds the line stream and the parse cursor. State transitions
// are encoded in the [parser.run] loop; the helper methods do not
// recurse, so the stack depth is bounded regardless of input size.
type parser struct {
	lines []string
	pos   int

	patch       *Patch
	currentFile *FilePatch
	currentHunk *Hunk
	hunkLine    int // 1-indexed line where the current hunk opened
}

// run executes the state machine and returns the populated [Patch] or
// the first parse error.
func (p *parser) run() (*Patch, error) {
	p.patch = &Patch{}
	p.skipLeadingBlanks()
	if err := p.expectBegin(); err != nil {
		return nil, err
	}
	for p.pos < len(p.lines) {
		line := p.lines[p.pos]
		lineNo := p.pos + 1
		p.pos++
		done, err := p.step(line, lineNo)
		if err != nil {
			return nil, err
		}
		if done {
			return p.patch, p.checkTrailing()
		}
	}
	return nil, &ParseError{Line: len(p.lines) + 1, Msg: ErrMissingEnd.Error(), Err: ErrMissingEnd}
}

// skipLeadingBlanks advances past empty lines before the begin
// directive so a model that prefixes its tool argument with a stray
// newline doesn't fail parse.
func (p *parser) skipLeadingBlanks() {
	for p.pos < len(p.lines) && strings.TrimSpace(p.lines[p.pos]) == "" {
		p.pos++
	}
}

// expectBegin consumes the `*** Begin Patch` header or returns
// [ErrMissingBegin].
func (p *parser) expectBegin() error {
	if p.pos >= len(p.lines) {
		return &ParseError{Line: 0, Msg: ErrMissingBegin.Error(), Err: ErrMissingBegin}
	}
	if strings.TrimSpace(p.lines[p.pos]) != "*** Begin Patch" {
		return &ParseError{
			Line: p.pos + 1,
			Msg:  fmt.Sprintf("%s (saw %q)", ErrMissingBegin.Error(), p.lines[p.pos]),
			Err:  ErrMissingBegin,
		}
	}
	p.pos++
	return nil
}

// step processes one input line. Returns done=true when the envelope
// terminator was consumed; the caller then validates trailing input.
func (p *parser) step(line string, lineNo int) (done bool, err error) {
	switch {
	case strings.TrimSpace(line) == "*** End Patch":
		return true, p.finishFile(lineNo)
	case strings.HasPrefix(line, "*** "):
		return false, p.handleDirective(line, lineNo)
	case strings.HasPrefix(line, "@@"):
		return false, p.openHunk(line, lineNo)
	default:
		return false, p.appendHunkLine(line, lineNo)
	}
}

// handleDirective dispatches `*** ...` lines other than Begin/End.
func (p *parser) handleDirective(line string, lineNo int) error {
	switch {
	case strings.HasPrefix(line, "*** Update File:"):
		return p.openFile(line, lineNo)
	case strings.HasPrefix(line, "*** Add File:"):
		return &ParseError{Line: lineNo, Msg: (&UnsupportedV2Error{Directive: "Add File"}).Error(), Err: &UnsupportedV2Error{Directive: "Add File"}}
	case strings.HasPrefix(line, "*** Delete File:"):
		return &ParseError{Line: lineNo, Msg: (&UnsupportedV2Error{Directive: "Delete File"}).Error(), Err: &UnsupportedV2Error{Directive: "Delete File"}}
	default:
		return &ParseError{Line: lineNo, Msg: fmt.Sprintf("%s: %q", ErrUnknownDirective.Error(), line), Err: ErrUnknownDirective}
	}
}

// openFile transitions to a new FilePatch. v1 rejects a second
// `*** Update File` directive with [ErrMultiFile].
func (p *parser) openFile(line string, lineNo int) error {
	if err := p.closeHunk(); err != nil {
		return err
	}
	if p.currentFile != nil {
		return &ParseError{Line: lineNo, Msg: ErrMultiFile.Error(), Err: ErrMultiFile}
	}
	path := strings.TrimSpace(strings.TrimPrefix(line, "*** Update File:"))
	if path == "" {
		return &ParseError{Line: lineNo, Msg: ErrMissingPath.Error(), Err: ErrMissingPath}
	}
	p.currentFile = &FilePatch{Path: path}
	return nil
}

// openHunk closes any in-progress hunk and starts a new one with the
// anchor text from the `@@ ...` line.
func (p *parser) openHunk(line string, lineNo int) error {
	if p.currentFile == nil {
		return &ParseError{Line: lineNo, Msg: ErrNoFile.Error(), Err: ErrNoFile}
	}
	if err := p.closeHunk(); err != nil {
		return err
	}
	anchor := strings.TrimSpace(strings.TrimPrefix(line, "@@"))
	p.currentHunk = &Hunk{Anchor: anchor}
	p.hunkLine = lineNo
	return nil
}

// appendHunkLine classifies a non-directive line as context, delete,
// or insert and appends it to the current hunk. A line that arrives
// before any hunk has been opened starts an implicit hunk so a patch
// without an explicit `@@` marker still parses.
func (p *parser) appendHunkLine(line string, lineNo int) error {
	if p.currentFile == nil {
		return &ParseError{Line: lineNo, Msg: ErrNoFile.Error(), Err: ErrNoFile}
	}
	if p.currentHunk == nil {
		p.currentHunk = &Hunk{}
		p.hunkLine = lineNo
	}
	kind, text := classifyHunkLine(line)
	p.currentHunk.Lines = append(p.currentHunk.Lines, HunkLine{Kind: kind, Text: text})
	return nil
}

// closeHunk validates the current hunk has at least one -/+ line and
// appends it to the current file. Calling closeHunk when no hunk is
// open is a no-op so callers can invoke it defensively before opening
// a new file or hunk.
func (p *parser) closeHunk() error {
	if p.currentHunk == nil {
		return nil
	}
	if !hasEdit(*p.currentHunk) {
		return &ParseError{Line: p.hunkLine, Msg: ErrEmptyHunk.Error(), Err: ErrEmptyHunk}
	}
	p.currentFile.Hunks = append(p.currentFile.Hunks, *p.currentHunk)
	p.currentHunk = nil
	return nil
}

// finishFile closes the trailing hunk and commits the current file to
// the patch. Called when `*** End Patch` is consumed.
func (p *parser) finishFile(endLine int) error {
	if err := p.closeHunk(); err != nil {
		return err
	}
	if p.currentFile == nil {
		return &ParseError{Line: endLine, Msg: ErrNoFile.Error(), Err: ErrNoFile}
	}
	if len(p.currentFile.Hunks) == 0 {
		return &ParseError{Line: endLine, Msg: "no hunks in '*** Update File' section", Err: ErrEmptyHunk}
	}
	p.patch.Files = append(p.patch.Files, *p.currentFile)
	p.currentFile = nil
	return nil
}

// checkTrailing ensures nothing other than blank lines follows
// `*** End Patch`. Stray content past the terminator usually means
// the LLM concatenated two envelopes or appended explanation text;
// failing loud is friendlier than silently dropping it.
func (p *parser) checkTrailing() error {
	for p.pos < len(p.lines) {
		if strings.TrimSpace(p.lines[p.pos]) != "" {
			return &ParseError{
				Line: p.pos + 1,
				Msg:  fmt.Sprintf("unexpected content after '*** End Patch': %q", p.lines[p.pos]),
				Err:  ErrUnknownDirective,
			}
		}
		p.pos++
	}
	return nil
}

// classifyHunkLine splits a hunk-body line into its kind and its
// content. The classification is lenient on context lines so a patch
// that omits the leading space (which models sometimes do for blank
// rows) still parses correctly:
//
//   - `-foo`  → ([HunkDelete],  "foo")
//   - `+foo`  → ([HunkInsert],  "foo")
//   - ` foo`  → ([HunkContext], "foo")  — one leading space stripped
//   - “      → ([HunkContext], "")     — blank line as context
//   - `foo`   → ([HunkContext], "foo")  — lenient bare context
func classifyHunkLine(line string) (HunkKind, string) {
	switch {
	case strings.HasPrefix(line, "-"):
		return HunkDelete, line[1:]
	case strings.HasPrefix(line, "+"):
		return HunkInsert, line[1:]
	case strings.HasPrefix(line, " "):
		return HunkContext, line[1:]
	default:
		return HunkContext, line
	}
}

// hasEdit reports whether the hunk contains at least one -/+ line.
// A hunk made of only context lines is an [ErrEmptyHunk].
func hasEdit(h Hunk) bool {
	for _, l := range h.Lines {
		if l.Kind == HunkDelete || l.Kind == HunkInsert {
			return true
		}
	}
	return false
}
