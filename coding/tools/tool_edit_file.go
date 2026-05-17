package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/coding/event"
)

// lineNumberPrefixRe matches the leading line-number prefix that the
// engine prepends when showing file content to the LLM. Two prompt
// surfaces produce these prefixes:
//
//   - buildMessages (engine/agent/prompt.go ~line 31) — format `%4d | %s`,
//     producing rows like `"   1 | content"`.
//   - sliceLines (engine/agent/tool_read_file.go ~line 138) — format
//     `%4d\t%s`, producing rows like `"   1\tcontent"`.
//
// When the model copies file content into edit_file's `search` field
// verbatim, the prefix comes along too — and never matches the actual
// file. We strip it deterministically here rather than asking the model
// to remember not to include it (the prompt-only instruction was
// unreliable, especially on long context).
//
// Capture groups: $1 is the numeric prefix as text (e.g. "  12"), $2 is
// the separator (`|` or `\t`). [stripLineNumberPrefixes] uses both to
// validate that a candidate excerpt is a real prompt-style block: every
// line must share the same separator AND the captured numbers must form
// a contiguous +1 sequence. Without those checks, an unrelated digit-
// prefixed table (e.g. a markdown row "1 | A" / "3 | C") would get its
// prefixes silently shaved off.
var lineNumberPrefixRe = regexp.MustCompile(`^[ ]*(\d+)([ ]+\||\t)[ ]?`)

// editOverlapWarningThreshold is the unchanged-line ratio at which an
// edit_file call logs an over-rewrite warning. 0.80 means "if more than
// 80% of search lines also appear unchanged in replace, the model is
// rewriting too much for the change it intends to make." Tuned to flag
// the obvious "rewrote a whole function to add one field" pattern
// without firing on legitimate small edits where overlap is naturally
// high (most lines are context anchors).
const editOverlapWarningThreshold = 0.80

// editOverlapMinLines suppresses the over-rewrite warning for edits with
// fewer search lines than this floor. Tiny edits (a one-line change with
// two anchor lines) routinely sit above the ratio threshold but are
// exactly the right shape — flagging them would train developers to
// ignore the warning.
const editOverlapMinLines = 5

// editOverlapRatio reports what fraction of search's non-empty lines also
// appear unchanged in replace. Returns (ratio, searchLineCount). The
// ratio is computed line-by-line via a multiset intersection: each
// search line that has a counterpart in replace contributes once,
// duplicate matches are not double-counted. Empty lines are excluded
// from both numerator and denominator because they are noise — every
// edit has whitespace lines, and a cluster of empty lines would inflate
// the ratio without meaning anything.
//
// A ratio of 1.0 means every non-empty search line has a duplicate in
// replace (extreme over-rewrite). A ratio of 0.0 means the replace
// shares no lines with search (clean rewrite or wholly new content).
// Returns (0, 0) for empty search to keep callers from dividing by
// zero.
func editOverlapRatio(search, replace string) (float64, int) {
	searchLines := nonEmptyLines(search)
	if len(searchLines) == 0 {
		return 0, 0
	}
	replaceMultiset := make(map[string]int, len(searchLines))
	for _, line := range nonEmptyLines(replace) {
		replaceMultiset[line]++
	}
	matched := 0
	for _, line := range searchLines {
		if replaceMultiset[line] > 0 {
			replaceMultiset[line]--
			matched++
		}
	}
	return float64(matched) / float64(len(searchLines)), len(searchLines)
}

// nonEmptyLines splits s on \n and returns only the lines whose trimmed
// content is non-empty. Used by [editOverlapRatio] so leading/trailing
// blank lines and indentation-only rows do not skew the overlap signal.
func nonEmptyLines(s string) []string {
	if s == "" {
		return nil
	}
	raw := strings.Split(s, "\n")
	out := raw[:0]
	for _, line := range raw {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// stripLineNumberPrefixes removes the leading line-number prefix from
// every line of s and returns the cleaned text. The strip applies ONLY
// when the input is a real prompt-style excerpt — three conditions must
// all hold:
//
//  1. Every non-blank line carries a recognisable prefix.
//  2. Every non-blank line uses the same separator (all `|` or all `\t`,
//     not mixed). A genuine excerpt comes from one of buildMessages or
//     sliceLines, never both — mixed separators imply the content was
//     hand-assembled.
//  3. The captured line numbers form a contiguous increasing sequence
//     (each number is exactly previous+1). Real prompt excerpts always
//     show consecutive lines; a table that happens to look like
//     prefixes ("1 | A" / "3 | C" / "5 | E") will not pass this check.
//
// Any failure returns s unchanged. Blank lines are excluded from all
// three checks because they carry no prefix in either format and would
// otherwise force the all-or-nothing rule to fail spuriously.
func stripLineNumberPrefixes(s string) string {
	if s == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	separator := ""
	prevNum := 0
	prefixed := false
	for _, line := range lines {
		if line == "" {
			continue
		}
		m := lineNumberPrefixRe.FindStringSubmatch(line)
		if m == nil {
			return s // condition 1 violated — at least one line lacks a prefix
		}
		num, err := strconv.Atoi(m[1])
		if err != nil {
			return s // theoretically unreachable (regex matched \d+) but defensive
		}
		sep := m[2]
		if !prefixed {
			separator = sep
			prevNum = num
			prefixed = true
			continue
		}
		if sep != separator {
			return s // condition 2 violated — separator mismatch
		}
		if num != prevNum+1 {
			return s // condition 3 violated — non-contiguous numbers
		}
		prevNum = num
	}
	if !prefixed {
		return s
	}
	for i, line := range lines {
		lines[i] = lineNumberPrefixRe.ReplaceAllString(line, "")
	}
	return strings.Join(lines, "\n")
}

// maxContentPreview is the max bytes of file content included in error messages
// sent back to the LLM. Prevents unbounded message sizes for large files.
const maxContentPreview = 8 * 1024

// maxDiffPreview is the max bytes of diff output included in recalibration
// messages. Caps the SimpleDiff output to avoid blowing token budgets.
const maxDiffPreview = 4 * 1024

// TruncateForPreview returns content truncated for LLM context windows.
// The suffix hints the LLM how to retrieve the full content.
func TruncateForPreview(content string) string {
	return TruncateWithHint(content, "use read_file for full content")
}

// TruncateWithHint truncates content at maxContentPreview with a custom hint.
func TruncateWithHint(content, hint string) string {
	if len(content) <= maxContentPreview {
		return content
	}
	return content[:maxContentPreview] + "\n\n[... truncated — " + hint + "]"
}

// maxDiffInputBytes caps the combined input size to SimpleDiff.
// Files beyond this threshold get a placeholder instead of a line-level diff.
const maxDiffInputBytes = 10 * 1024 * 1024

// maxToolArgsBytes caps the raw JSON payload of a single tool call's
// arguments before unmarshal. Sized to comfortably contain the largest
// well-formed call (two maxDiffInputBytes-sized strings plus JSON
// overhead) while protecting against pathological LLM output (runaway
// repetition in a single call) that would otherwise allocate gigabytes
// during decode. Per-field caps still apply after unmarshal — this is
// the early bouncer.
const maxToolArgsBytes = 32 * 1024 * 1024

// SimpleDiff produces a unified-diff-like comparison between expected and actual
// content, showing only the lines that differ. Output is capped at maxDiffPreview
// bytes to avoid blowing token budgets on large file changes.
func SimpleDiff(expected, actual string) string {
	if len(expected)+len(actual) > maxDiffInputBytes {
		return "(diff omitted: content too large)"
	}
	expectedLines := strings.Split(expected, "\n")
	actualLines := strings.Split(actual, "\n")

	var b diffBuilder
	ei, ai := 0, 0
	for ei < len(expectedLines) || ai < len(actualLines) {
		if ei < len(expectedLines) && ai < len(actualLines) && expectedLines[ei] == actualLines[ai] {
			ei++
			ai++
			continue
		}
		matchAhead := findMatch(expectedLines, actualLines, ei, ai)
		if matchAhead.found {
			ei, ai = b.writeHunk(expectedLines, actualLines, ei, matchAhead.ei, ai, matchAhead.ai)
		} else {
			ei, ai = b.writeHunk(expectedLines, actualLines, ei, len(expectedLines), ai, len(actualLines))
		}
		if b.truncated {
			break
		}
	}

	result := b.buf.String()
	if b.truncated && result == "" {
		return "[... diff truncated]"
	}
	if result == "" {
		return "(whitespace-only changes)"
	}
	if b.truncated {
		result += "\n[... diff truncated]"
	}
	return result
}

// diffBuilder accumulates diff lines with a size cap.
type diffBuilder struct {
	buf       strings.Builder
	truncated bool
}

// writeLine appends a diff line if under the cap. Returns false if truncated.
func (d *diffBuilder) writeLine(prefix, line string) bool {
	entry := prefix + " " + line + "\n"
	if d.buf.Len()+len(entry) > maxDiffPreview {
		d.truncated = true
		return false
	}
	d.buf.WriteString(entry)
	return true
}

// writeHunk writes removed lines [ei:endE) and added lines [ai:endA).
// Returns the new ei, ai positions.
func (d *diffBuilder) writeHunk(expected, actual []string, ei, endE, ai, endA int) (int, int) {
	for ; ei < endE; ei++ {
		if !d.writeLine("-", expected[ei]) {
			return ei, ai
		}
	}
	for ; ai < endA; ai++ {
		if !d.writeLine("+", actual[ai]) {
			return ei, ai
		}
	}
	return ei, ai
}

type matchResult struct {
	found  bool
	ei, ai int
}

// findMatch scans ahead to find the next line where expected and actual re-sync.
// Limited lookahead to avoid O(n²) on large files.
func findMatch(expected, actual []string, ei, ai int) matchResult {
	const maxLookahead = 20
	limitE := min(ei+maxLookahead, len(expected))
	limitA := min(ai+maxLookahead, len(actual))

	for de := 0; de < limitE-ei; de++ {
		for da := 0; da < limitA-ai; da++ {
			if de == 0 && da == 0 {
				continue // skip current position
			}
			if expected[ei+de] == actual[ai+da] {
				return matchResult{found: true, ei: ei + de, ai: ai + da}
			}
		}
	}
	return matchResult{}
}

// changedLineRanges returns the 1-indexed inclusive line ranges in
// `final` that differ from `orig`. Used by the post-edit content
// formatter to render slices ±N lines around the modified regions when
// the file is too large to include in full.
//
// The walk mirrors [SimpleDiff]: equal lines advance both indices in
// lockstep; a divergence is resolved by [findMatch]'s bounded
// lookahead. The lookahead means very large insert-or-delete blocks
// past 20 lines without a re-sync collapse into a single "changed
// through end" range — that's conservative (we show more context than
// strictly necessary) but never wrong.
//
// Empty `orig` (i.e. new file) returns one range covering all of
// `final`. Empty `final` returns nil (no lines to slice around). An
// unchanged file returns nil.
func changedLineRanges(orig, final string) []LineRange {
	if orig == final {
		return nil
	}
	// Empty final returns nil; the formatter short-circuits on
	// empty content with a "file now empty" placeholder, so ranges
	// would never be used. Keeping this path nil matches the caller
	// contract — strings.Split("", "\n") would otherwise produce
	// a single-element [""] slice that walks to a spurious {1,1}.
	if final == "" {
		return nil
	}
	finalLines := strings.Split(final, "\n")
	if orig == "" {
		return []LineRange{{Start: 1, End: len(finalLines)}}
	}
	origLines := strings.Split(orig, "\n")

	var ranges []LineRange
	oi, fi := 0, 0
	for oi < len(origLines) || fi < len(finalLines) {
		if oi < len(origLines) && fi < len(finalLines) && origLines[oi] == finalLines[fi] {
			oi++
			fi++
			continue
		}
		startF := fi
		match := findMatch(origLines, finalLines, oi, fi)
		if match.found {
			oi, fi = match.ei, match.ai
		} else {
			oi, fi = len(origLines), len(finalLines)
		}
		if fi > startF {
			ranges = append(ranges, LineRange{Start: startF + 1, End: fi})
		} else if startF == fi && oi > 0 {
			// Pure deletion at this position — anchor the touched
			// range on the line immediately after the deletion seam
			// in the final buffer (1-indexed). startF is the
			// 0-indexed position where divergence began, so the line
			// AFTER the seam in 1-indexed terms is startF + 1. This
			// matches the insert/replace branch's convention
			// (Start: startF + 1) and gives the slice renderer the
			// correct anchor to expand context around. The clamp to
			// len(finalLines) handles tail deletions where startF
			// already equals the file's final line.
			anchor := startF + 1
			if anchor < 1 {
				anchor = 1
			}
			if anchor > len(finalLines) {
				anchor = len(finalLines)
			}
			ranges = append(ranges, LineRange{Start: anchor, End: anchor})
		}
	}
	return ranges
}

// postEditContentBudget is the byte budget the post-approval body
// reserves for the file-content blob. Matches the legacy
// [maxContentPreview] cap (8 KiB) so the per-edit history payload
// does not grow vs the pre-PR-#157 baseline; the line-number format
// upgrade still pays off (the LLM reads tool results in the same
// shape as read_file's sliceLines output), but the size does not.
//
// History impact, locked 2026-05-17 after pacman re-run: a 30 KiB
// budget here caused tier-2 compaction to fire 12x in a 32-turn
// session (vs 5x in the 31-turn baseline) because each successful
// edit injected a 5–15 KiB blob into history, blowing past the
// 30 KiB tier-2 threshold every 2–3 turns. Each tier-2 firing
// burns ~12K input + resets the prompt cache for the following
// real turn — the dominant cost spike vs baseline. Sizing the
// budget at 8 KiB brings the per-edit history payload back to
// roughly the legacy cap; the line-number format change remains.
//
// Large files (rendered > 8 KiB) fall through to the slice mode
// where each touched range gets ±[postEditSliceContext] lines and
// the slice budget itself is enforced here.
const postEditContentBudget = 8 * 1024

// postEditSliceContext is the number of lines included on each side
// of a touched range when slicing a large file. Matches the plan's
// "±20 lines around each touched range" contract.
const postEditSliceContext = 20

// FormatPostEditContent renders the post-edit file content for the
// LLM-facing tool result. Small files (rendered size within
// [postEditContentBudget]) are returned in full with cat -n style
// line numbers matching read_file's slice format. Larger files
// collapse to one or more slices ±[postEditSliceContext] lines
// around each touched range, merged when they overlap; the slice
// headers report the line range and total file size so the LLM can
// re-orient without a separate read_file call.
//
// touchedRanges may be nil (whole-file write) — in that case the
// formatter treats the entire file as touched: small files (rendered
// within [postEditContentBudget]) are returned in full; files that
// exceed the budget fall back to a head slice with a read_file hint
// because [mergeAndPadRanges] returns nil for nil input and there's no
// natural slice center to expand around. The LLM sees only the file's
// head in that case — large `write_file` scaffolds therefore won't
// echo their entire content back through the tool result. An empty
// content blob returns a short placeholder rather than nothing so the
// LLM sees a definite "file now empty" signal.
func FormatPostEditContent(path, content string, touchedRanges []LineRange) string {
	if content == "" {
		return fmt.Sprintf("Resulting file (%s) is now empty.", path)
	}
	lines := strings.Split(content, "\n")
	total := len(lines)
	// strings.Split on a trailing newline produces a final empty
	// entry; treat it as part of the previous line for display so
	// "Lines 1–N of N" matches what `wc -l` would say.
	display := total
	if total > 0 && lines[total-1] == "" {
		display = total - 1
	}

	full := renderLineRange(path, lines, 1, display, display)
	if len(full) <= postEditContentBudget {
		return full
	}

	// Large file — render slices around touched ranges.
	ranges := mergeAndPadRanges(touchedRanges, display, postEditSliceContext)
	if len(ranges) == 0 {
		// No touched ranges supplied (or all collapsed away). Fall
		// back to a head slice so the LLM at least sees the file
		// start; a marker advises read_file for the rest.
		head := renderLineRange(path, lines, 1, min(display, postEditSliceContext*2), display)
		return head + fmt.Sprintf("\n[file is %d lines and exceeds the per-result content budget; use read_file with offset/limit for the rest]", display)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Resulting file (%s, %d lines) — slices around modified regions:\n", path, display)
	used := b.Len()
	for i, r := range ranges {
		slice := renderLineRange(path, lines, r.Start, r.End, display)
		if used+len(slice) > postEditContentBudget {
			remaining := len(ranges) - i
			fmt.Fprintf(&b, "\n[%d additional slice(s) omitted to fit the per-result content budget; use read_file with offset/limit to view them]", remaining)
			break
		}
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(slice)
		used = b.Len()
	}
	return b.String()
}

// renderLineRange formats a [start, end] (1-indexed, inclusive) slice
// of `lines` with read_file's cat -n line-number style and a header
// naming the range and the total. The header matches sliceLines'
// format so a follow-up read_file with the same range produces a
// near-identical payload — the LLM doesn't have to learn two layouts.
func renderLineRange(path string, lines []string, start, end, total int) string {
	if start < 1 {
		start = 1
	}
	if end > total {
		end = total
	}
	if end < start {
		return fmt.Sprintf("Lines %d–%d of %d in %s\n(empty range)\n", start, end, total, path)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Lines %d–%d of %d in %s\n", start, end, total, path)
	for i := start - 1; i < end; i++ {
		fmt.Fprintf(&b, "%4d\t%s\n", i+1, lines[i])
	}
	return b.String()
}

// mergeAndPadRanges expands each range by `pad` lines on either side,
// clamps to [1, total], and merges overlapping or touching ranges.
// Returns ranges sorted by Start. A nil or empty input returns nil.
func mergeAndPadRanges(ranges []LineRange, total, pad int) []LineRange {
	if len(ranges) == 0 || total <= 0 {
		return nil
	}
	padded := make([]LineRange, 0, len(ranges))
	for _, r := range ranges {
		s := r.Start - pad
		if s < 1 {
			s = 1
		}
		e := r.End + pad
		if e > total {
			e = total
		}
		if s > total {
			continue
		}
		if e < 1 {
			continue
		}
		padded = append(padded, LineRange{Start: s, End: e})
	}
	if len(padded) == 0 {
		return nil
	}
	sort.Slice(padded, func(i, j int) bool { return padded[i].Start < padded[j].Start })
	merged := []LineRange{padded[0]}
	for _, r := range padded[1:] {
		last := &merged[len(merged)-1]
		if r.Start <= last.End+1 {
			if r.End > last.End {
				last.End = r.End
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

// EditFileTool lets the LLM propose search-and-replace edits to files.
// It validates the search text and submits the proposal through
// [Approver]; the approver runs the validation pipeline, sends
// the proposal to the frontend, and blocks on approval+continue.
type EditFileTool struct {
	workspace FileReader
	cache     *FileCache
	approver  Approver

	mu               sync.Mutex
	silentRetries    int
	maxSilentRetries int
}

// NewEditFileTool creates an EditFileTool with the given dependencies.
// The approver mediates the approval flow.
func NewEditFileTool(ws FileReader, cache *FileCache, approver Approver) *EditFileTool {
	return &EditFileTool{
		workspace:        ws,
		cache:            cache,
		approver:         approver,
		maxSilentRetries: 3,
	}
}

// Reset clears silent retry state between runs. Safe to call while
// a previous Execute() is unwinding after cancellation.
func (t *EditFileTool) Reset() {
	t.mu.Lock()
	t.silentRetries = 0
	t.mu.Unlock()
}

// Definition returns the tool definition for the LLM.
func (t *EditFileTool) Definition() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "edit_file",
			Description: "Exact search-and-replace in a file. Prefer the `edits` array for multi-spot patches in one call (each entry has its own `search`/`replace` and is applied in document order against the running buffer; atomic — if any entry fails, none apply). Use the top-level `search`/`replace` only for a single change. search text must match verbatim (whitespace and newlines included). Keep each search SHORT — only changing lines plus minimal context. Don't rewrite a whole function for a few-line change. Empty replace deletes; to insert, anchor with surrounding text and repeat the anchor in replace with the new code added.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"path": {
						Type:        "string",
						Description: "File path relative to project root.",
					},
					"search": {
						Type:        "string",
						Description: "Single-edit form: exact text to find. Ignored when `edits` is non-empty.",
					},
					"replace": {
						Type:        "string",
						Description: "Single-edit form: replacement text. Empty string to delete. Ignored when `edits` is non-empty.",
					},
					"edits": {
						Type:        "array",
						Description: "Multi-edit form: each entry is {search, replace}, applied in document order against the running buffer. Atomic — if any entry fails its search match or safety check, the whole call fails and no edits apply. Prefer this form whenever you have more than one change in the same file.",
						Items: &llm.FunctionParam{
							Type: "object",
							Properties: map[string]llm.FunctionParam{
								"search": {
									Type:        "string",
									Description: "Exact text to find in the running buffer. Must match verbatim.",
								},
								"replace": {
									Type:        "string",
									Description: "Replacement text. Empty string to delete the matched search text.",
								},
							},
							Required: []string{"search", "replace"},
						},
					},
					"reason": {
						Type:        "string",
						Description: "Brief explanation of the change, shown to the developer.",
					},
				},
				Required: []string{"path", "reason"},
			},
		},
	}
}

// editSpec is one search/replace pair in the multi-edit `edits` array.
// Per-spec failures fail the whole edit_file call (atomicity); position
// in the failure error names the 0-based index so the LLM can correct.
type editSpec struct {
	Search  string `json:"search"`
	Replace string `json:"replace"`
}

type editArgs struct {
	Path    string     `json:"path"`
	Search  string     `json:"search"`
	Replace string     `json:"replace"`
	Edits   []editSpec `json:"edits"`
	Reason  string     `json:"reason"`
}

// Execute validates the edit and submits an [EditProposal] through the
// configured [Approver]. The approver runs the validation
// pipeline, sends the proposal to the frontend, and blocks on
// approval+continue; its returned body becomes the tool result.
//
// Two argument shapes are supported. The single-edit form uses the
// top-level `search`/`replace` fields and preserves the legacy silent-
// retry budget on a search mismatch (handy for the LLM correcting
// near-miss whitespace on its own turn). The multi-edit form passes an
// `edits` array; the call is atomic — every entry is checked against a
// running in-memory buffer in order, and the first failure aborts the
// whole batch with a structured error naming the failed index. No
// partial applies, no silent-retry budget consumed.
func (t *EditFileTool) Execute(ctx context.Context, call llm.ToolCall) ToolResult {
	if len(call.Function.Arguments) > maxToolArgsBytes {
		return errorResult(fmt.Sprintf("Error: arguments too large (%d bytes, max %d). Use a narrower edit.", len(call.Function.Arguments), maxToolArgsBytes))
	}
	var args editArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return errorResult(fmt.Sprintf("Error: invalid arguments: %v", err))
	}
	if args.Path == "" {
		return errorResult("Error: path is required")
	}

	content, canon, err := t.resolveContent(args.Path)
	if err != nil {
		return errorResult(fmt.Sprintf("Error: cannot read %s: %v", args.Path, err))
	}

	if len(args.Edits) > 0 {
		return t.executeBatch(ctx, call, args, content, canon)
	}
	return t.executeSingle(ctx, call, args, content, canon)
}

// executeSingle handles the legacy `{search, replace}` form. Behavior
// preserved verbatim from pre-batching: silent-retry budget on a
// missed search, fuzzy-whitespace fallback, per-edit corruption +
// over-rewrite checks, single [EditProposal] submission.
func (t *EditFileTool) executeSingle(ctx context.Context, call llm.ToolCall, args editArgs, content, canon string) ToolResult {
	if len(args.Search) > maxDiffInputBytes || len(args.Replace) > maxDiffInputBytes {
		return errorResult(fmt.Sprintf("Error: search/replace too large (max %d bytes each). Use a narrower edit.", maxDiffInputBytes))
	}
	if args.Search == "" && content != "" {
		return errorResult("Error: search field cannot be empty (file is not empty — copy existing text to anchor your edit)")
	}

	// Strip line-number prefixes before matching. The prefixes come from
	// our own prompt surfaces and are never present in actual file
	// content; copying them verbatim into `search` would guarantee a
	// no-match. Done before validateSearchMatch so the silent-retry
	// budget is not consumed by a prefix-induced mismatch.
	args.Search = stripLineNumberPrefixes(args.Search)

	correctedSearch, errMsg := t.validateSearchMatch(args.Path, args.Search, content)
	if errMsg != "" {
		return errorResult(errMsg)
	}
	if correctedSearch != "" {
		args.Search = correctedSearch
	}

	if msg := detectLikelyCorruption(args.Search, args.Replace, content); msg != "" {
		slog.Warn("edit_file: rejecting likely-corrupting edit",
			"path", args.Path, "reason", msg,
			"search_len", len(args.Search), "replace_len", len(args.Replace))
		return errorResult("Error: " + msg)
	}

	if ratio, lines := editOverlapRatio(args.Search, args.Replace); lines >= editOverlapMinLines && ratio >= editOverlapWarningThreshold {
		slog.Warn("edit_file: over-rewrite detected — search and replace mostly identical",
			"path", args.Path,
			"search_lines", lines,
			"unchanged_ratio", ratio,
			"hint", "split into smaller edits that only change the lines that differ")
	}

	expectedContent := strings.Replace(content, args.Search, args.Replace, 1)

	body, isErr := t.approver.Propose(ctx, EditProposal{
		Edit: event.PendingEdit{
			ID:      call.ID,
			Path:    args.Path,
			Search:  args.Search,
			Replace: args.Replace,
			Reason:  args.Reason,
		},
		Path:            args.Path,
		CanonPath:       canon,
		ExpectedContent: expectedContent,
		TouchedLines:    changedLineRanges(content, expectedContent),
	})
	return ToolResult{Content: body, IsError: isErr}
}

// executeBatch handles the multi-edit `edits` array. Each entry is
// validated and applied to a running in-memory buffer in order; any
// per-entry failure (search not found, ambiguous match, corruption
// guard) aborts the whole call with a structured error naming the
// failed index. Atomicity is structural: the [Approver] sees a single
// proposal covering the cumulative diff only when every entry passed.
//
// Differences from the single-edit path: no silent-retry budget (a
// missed search fails the batch immediately so the LLM gets a single
// clean retry rather than budget-eating partial applies), and no
// fuzzy-whitespace fallback (the multi-edit form is for the LLM that
// has the file content already and is patching N spots in one call —
// per-spot whitespace forgiveness encourages sloppier sub-edits).
func (t *EditFileTool) executeBatch(ctx context.Context, call llm.ToolCall, args editArgs, content, canon string) ToolResult {
	running := content
	for i, e := range args.Edits {
		if len(e.Search) > maxDiffInputBytes || len(e.Replace) > maxDiffInputBytes {
			return errorResult(fmt.Sprintf("Error: edits[%d] search/replace too large (max %d bytes each). Use a narrower edit.", i, maxDiffInputBytes))
		}
		if e.Search == "" && running != "" {
			return errorResult(fmt.Sprintf("Error: edits[%d] search field cannot be empty (file is not empty — copy existing text to anchor your edit)", i))
		}

		search := stripLineNumberPrefixes(e.Search)

		matchCount := strings.Count(running, search)
		if matchCount != 1 {
			reason := "search text not found in running buffer (after prior edits in this call were applied)"
			if matchCount > 1 {
				reason = fmt.Sprintf("search text matches %d locations in running buffer (expected exactly 1)", matchCount)
			}
			slog.Info("edit_file: batch entry failed search match",
				"path", args.Path, "index", i,
				"matches", matchCount, "search_len", len(search))
			return errorResult(fmt.Sprintf(
				"Error: edits[%d] failed: %s. No edits in this call were applied (atomic). Make the search text more specific, then retry the whole batch.\n\nCurrent file (%s):\n\n%s",
				i, reason, args.Path, TruncateForPreview(content)))
		}

		if msg := detectLikelyCorruption(search, e.Replace, running); msg != "" {
			slog.Warn("edit_file: batch entry rejected as likely-corrupting",
				"path", args.Path, "index", i, "reason", msg)
			return errorResult(fmt.Sprintf("Error: edits[%d]: %s No edits in this call were applied (atomic).", i, msg))
		}

		if ratio, lines := editOverlapRatio(search, e.Replace); lines >= editOverlapMinLines && ratio >= editOverlapWarningThreshold {
			slog.Warn("edit_file: batch entry over-rewrite detected — search and replace mostly identical",
				"path", args.Path, "index", i,
				"search_lines", lines,
				"unchanged_ratio", ratio,
				"hint", "split into smaller edits that only change the lines that differ")
		}

		running = strings.Replace(running, search, e.Replace, 1)
	}

	// Reset silent-retry state on successful atomic batch — the LLM
	// just demonstrated it can read the file accurately. The mu lock
	// matches the single-edit path's reset on a clean match.
	t.mu.Lock()
	t.silentRetries = 0
	t.mu.Unlock()

	// The frontend approval pane displays one diff for the whole
	// batch (Search = original full content, Replace = post-batch
	// full content). For multi-spot patches this matches what the
	// developer would see in a "show changes" overlay anyway, and
	// avoids the alternative of N consecutive approval prompts.
	body, isErr := t.approver.Propose(ctx, EditProposal{
		Edit: event.PendingEdit{
			ID:      call.ID,
			Path:    args.Path,
			Search:  content,
			Replace: running,
			Reason:  args.Reason,
		},
		Path:            args.Path,
		CanonPath:       canon,
		ExpectedContent: running,
		TouchedLines:    changedLineRanges(content, running),
	})
	return ToolResult{Content: body, IsError: isErr}
}

// resolveContent returns the file content and canonical path, reading
// from cache first and falling back to disk. The closure captures the
// original relative path so ReadFile receives the documented
// relative-path input while the cache is keyed by canonical absolute
// path.
//
// The cache is kept in sync with the editor buffer between agent
// turns: after each approved edit the orchestrator seeds the cache
// with the proposal's expected content under the canonical path. So
// in steady state, "cache content" is the same bytes
// Session.ReviewEdit will search against in the buffer.
//
// A previous CurrentContentReader-based fix tried to bypass the
// cache by reading the buffer directly. That introduced a
// cross-goroutine race against the TUI-owned buffer state, which
// the existing architecture deliberately avoids by doing buffer
// reads only on the TUI goroutine. Reverted in favour of the
// in-sync-cache invariant.
func (t *EditFileTool) resolveContent(path string) (content, canon string, err error) {
	canon = t.workspace.CanonPath(path)
	content, err = t.cache.LoadOrRead(canon, func() (string, error) {
		return t.workspace.ReadFile(path)
	})
	if err != nil {
		return "", canon, err
	}
	if len(content) > maxDiffInputBytes {
		t.cache.Invalidate(canon) // don't retain oversized files in cache
		return "", canon, fmt.Errorf("file too large (%d bytes, max %d)", len(content), maxDiffInputBytes)
	}
	return content, canon, nil
}

// validateSearchMatch checks that the search text appears exactly once in the
// file content. Returns (correctedSearch, "") on success — correctedSearch is
// non-empty when a whitespace-normalized fallback matched and the caller
// should use it instead of the original search text. Returns ("", errMsg)
// on failure, managing the silent retry counter.
func (t *EditFileTool) validateSearchMatch(path, search, content string) (string, string) {
	matchCount := strings.Count(content, search)
	if matchCount == 1 {
		t.mu.Lock()
		t.silentRetries = 0
		t.mu.Unlock()
		return "", ""
	}

	// Exact match failed — try whitespace-normalized fallback.
	// Normalizes leading whitespace on each line so tab/space mismatches
	// and wrong indentation depth don't block the edit.
	if matchCount == 0 {
		if corrected := fuzzyWhitespaceMatch(search, content); corrected != "" {
			slog.Info("edit_file: whitespace-normalized match", "path", path)
			t.mu.Lock()
			t.silentRetries = 0
			t.mu.Unlock()
			return corrected, ""
		}
	}

	t.mu.Lock()
	if t.silentRetries < t.maxSilentRetries {
		t.silentRetries++
		remaining := t.maxSilentRetries - t.silentRetries
		attempt := t.silentRetries
		t.mu.Unlock()

		errMsg := "search text not found in file"
		if matchCount > 1 {
			errMsg = fmt.Sprintf("search text matches %d locations (expected exactly 1) — make the search text more specific", matchCount)
		}
		slog.Info("edit_file: silent retry", "reason", errMsg,
			"attempt", attempt, "path", path, "search_len", len(search))
		return "", fmt.Sprintf("Error: %s. You have %d retries left. Read the file content carefully and copy the exact text.\n\nCurrent file (%s):\n\n%s",
			errMsg, remaining, path, TruncateForPreview(content))
	}

	t.silentRetries = 0
	t.mu.Unlock()
	slog.Warn("edit_file: validation failed after max retries",
		"matches", matchCount, "path", path, "search_len", len(search))
	return "", fmt.Sprintf("Error: search text validation failed after %d retries. Use read_file to re-read the file and copy the exact text.\n\nCurrent file (%s):\n\n%s",
		t.maxSilentRetries, path, TruncateForPreview(content))
}

// midLineTruncationFloor is the minimum replace length at which a mid-line
// end (no trailing newline) is treated as a likely LLM output-token cutoff.
// Short inline edits legitimately don't end in newlines; long ones almost
// always do when the surrounding search ends with one.
const midLineTruncationFloor = 512

// duplicationProbeBytes is the suffix length of replace compared against
// content-after-search to detect the "agent rewrites an already-present tail"
// pattern. Must be long enough that coincidental matches are vanishingly rare
// in real code, short enough that small but real duplications are still caught.
// 64 bytes comfortably spans several lines of real code while remaining below
// the typical size of reconstituted file tails the agent tries to "restore".
const duplicationProbeBytes = 64

// detectLikelyCorruption returns a non-empty error message when the proposed
// edit matches a known file-corrupting pattern that the LLM almost never
// intends. Two patterns are caught:
//
//  1. Mid-line truncation: search ends with a newline but replace does not,
//     and replace is long enough that a mid-line end strongly suggests the
//     LLM hit its output token cap mid-generation. Applying it would silently
//     shorten the file and fuse two lines.
//
//  2. Suffix duplication: a long suffix of replace also appears in the file
//     content immediately after the search match. Applying the edit would
//     leave that content present in both the new replace block and the
//     unchanged tail — the classic cascade that turns one truncation into
//     repeated duplicated blocks across subsequent repair attempts.
//
// Returns "" when the edit looks safe. Callers should surface the returned
// message back to the LLM so it can retry with a narrower edit.
func detectLikelyCorruption(search, replace, content string) string {
	if replace == "" {
		return "" // plain deletion — neither pattern applies
	}
	if strings.HasSuffix(search, "\n") && !strings.HasSuffix(replace, "\n") &&
		len(replace) >= midLineTruncationFloor {
		return "replace appears truncated mid-line — search ends with a newline but replace does not, and replace is large (likely an LLM output-token cutoff). Retry with a much narrower edit covering only the lines that actually change."
	}

	idx := strings.Index(content, search)
	if idx < 0 {
		return "" // validateSearchMatch already guarantees a single match
	}
	afterSearch := content[idx+len(search):]
	if len(replace) >= duplicationProbeBytes && len(afterSearch) >= duplicationProbeBytes {
		probe := replace[len(replace)-duplicationProbeBytes:]
		// Only scan the window immediately adjacent to the match. Real
		// boundary-crossing duplication puts the duplicated bytes within
		// ~len(replace) of the seam; matches further out are almost always
		// coincidental repetitions of common code shapes (e.g. error-return
		// idioms) and would otherwise produce false positives.
		lookahead := min(len(afterSearch), len(replace)+duplicationProbeBytes)
		if strings.Contains(afterSearch[:lookahead], probe) {
			return "replace duplicates content that already exists after the match point — applying this edit would leave the same block present twice in the file. Narrow the search/replace to only the lines that actually change instead of rewriting surrounding context that is already there."
		}
	}
	return ""
}

// fuzzyWhitespaceMatch attempts to find the search text in content by
// normalizing leading whitespace on each line. If exactly one match is
// found, returns the actual text from the file that matched. Returns ""
// if no match or multiple matches.
func fuzzyWhitespaceMatch(search, content string) string {
	searchLines := strings.Split(search, "\n")

	// Build a pattern from the search text with leading whitespace stripped.
	stripped := make([]string, len(searchLines))
	for i, line := range searchLines {
		stripped[i] = strings.TrimLeft(line, " \t")
	}

	// Scan content lines for a contiguous block that matches when
	// leading whitespace is stripped.
	contentLines := strings.Split(content, "\n")
	var matches []string
	for i := 0; i <= len(contentLines)-len(searchLines); i++ {
		match := true
		for j, want := range stripped {
			got := strings.TrimLeft(contentLines[i+j], " \t")
			if got != want {
				match = false
				break
			}
		}
		if match {
			if len(matches) == 1 {
				return ""
			}
			actual := strings.Join(contentLines[i:i+len(searchLines)], "\n")
			matches = append(matches, actual)
		}
	}

	if len(matches) == 1 && matches[0] != search {
		return matches[0]
	}
	return ""
}
