package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
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
			Description: "Search for exact text in a file and replace it. The search string must match the file content exactly (including whitespace and newlines). The replace string must be correctly formatted code with proper indentation matching the file's style — never collapse multiple lines onto one line. Keep search text as SHORT as possible — only include lines that actually change, plus minimal context to match uniquely. Do NOT rewrite entire functions when only a few lines change. To delete text, set replace to an empty string. To insert, include anchor text in search and repeat it in replace with the new code added.",
			Parameters: llm.FunctionParams{
				Type: "object",
				Properties: map[string]llm.FunctionParam{
					"path": {
						Type:        "string",
						Description: "File path relative to project root.",
					},
					"search": {
						Type:        "string",
						Description: "Exact text to find in the file. Must match verbatim.",
					},
					"replace": {
						Type:        "string",
						Description: "Text to replace the search text with. Empty string to delete.",
					},
					"reason": {
						Type:        "string",
						Description: "Brief explanation of the change, shown to the developer.",
					},
				},
				Required: []string{"path", "search", "replace", "reason"},
			},
		},
	}
}

type editArgs struct {
	Path    string `json:"path"`
	Search  string `json:"search"`
	Replace string `json:"replace"`
	Reason  string `json:"reason"`
}

// Execute validates the edit and submits an [EditProposal] through the
// configured [Approver]. The approver runs the validation
// pipeline, sends the proposal to the frontend, and blocks on
// approval+continue; its returned body becomes the tool result.
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
	if len(args.Search) > maxDiffInputBytes || len(args.Replace) > maxDiffInputBytes {
		return errorResult(fmt.Sprintf("Error: search/replace too large (max %d bytes each). Use a narrower edit.", maxDiffInputBytes))
	}

	content, canon, err := t.resolveContent(args.Path)
	if err != nil {
		return errorResult(fmt.Sprintf("Error: cannot read %s: %v", args.Path, err))
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
