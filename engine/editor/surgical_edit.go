package editor

import "strings"

// NarrowedEdit describes the narrowed change region for surgical animation.
// It contains only the lines that actually differ — unchanged prefix and
// suffix lines are excluded so they never disappear from the buffer.
type NarrowedEdit struct {
	// Line is the buffer line where the change region starts (0-indexed).
	Line int
	// Col is the buffer column where the change region starts (0-indexed).
	Col int
	// Search is the text to delete (only the changed lines).
	Search string
	// Replace is the text to type (only the changed lines).
	Replace string
	// LineOrigins holds provenance for the replacement lines.
	// Indexed relative to the change region (not the full replace).
	LineOrigins []*LineOrigin
	// PrefixLines is the number of unchanged lines skipped at the top.
	PrefixLines int
	// SuffixLines is the number of unchanged lines skipped at the bottom.
	SuffixLines int
}

// NarrowEdit computes the narrowed change region from a full search/replace
// and its hunks. Unchanged prefix and suffix lines are excluded. The result
// can be passed directly to BeginIncrementalEdit for animation.
//
// editLine and editCol are the buffer position where the full search text
// starts (from LocateEdit). lineOrigins is the full per-line origin slice.
func NarrowEdit(
	editLine, editCol int,
	search, replace string,
	hunks []Hunk,
	lineOrigins []*LineOrigin,
) NarrowedEdit {
	searchLines := strings.Split(search, "\n")
	replaceLines := strings.Split(replace, "\n")

	// Count unchanged prefix/suffix lines from hunks.
	prefix, suffix := unchangedBounds(hunks, len(searchLines), len(replaceLines))

	// A zero-length delete causes the replacement to be inserted into the
	// existing line content at the insertion point rather than before it.
	// Include one boundary line so BeginIncrementalEdit always has text to
	// delete, keeping the animation result consistent with strings.Replace.
	if prefix+suffix == len(searchLines) {
		if prefix > 0 {
			prefix--
		} else if suffix > 0 {
			suffix--
		}
	}

	// Narrow the search text.
	narrowSearch := strings.Join(searchLines[prefix:len(searchLines)-suffix], "\n")

	// Narrow the replace text.
	narrowReplace := strings.Join(replaceLines[prefix:len(replaceLines)-suffix], "\n")

	// Narrow the line origins.
	var narrowOrigins []*LineOrigin
	if len(lineOrigins) > 0 {
		end := max(len(lineOrigins)-suffix, 0)
		if prefix < end {
			narrowOrigins = lineOrigins[prefix:end]
		}
	}

	// Compute the buffer position of the change region.
	narrowLine := editLine + prefix
	narrowCol := 0
	if prefix == 0 {
		narrowCol = editCol
	}

	return NarrowedEdit{
		Line:        narrowLine,
		Col:         narrowCol,
		Search:      narrowSearch,
		Replace:     narrowReplace,
		LineOrigins: narrowOrigins,
		PrefixLines: prefix,
		SuffixLines: suffix,
	}
}

// unchangedBounds returns the number of unchanged prefix and suffix lines
// by inspecting the hunk sequence.
func unchangedBounds(hunks []Hunk, searchLen, replaceLen int) (prefix, suffix int) {
	// Leading HunkKeep → prefix.
	if len(hunks) > 0 && hunks[0].Op == HunkKeep {
		prefix = hunks[0].SearchCount
	}
	// Trailing HunkKeep → suffix.
	if len(hunks) > 1 && hunks[len(hunks)-1].Op == HunkKeep {
		suffix = hunks[len(hunks)-1].SearchCount
	}
	// Clamp: prefix + suffix cannot exceed either side's line count.
	if prefix+suffix > searchLen {
		suffix = searchLen - prefix
	}
	if prefix+suffix > replaceLen {
		suffix = replaceLen - prefix
	}
	if suffix < 0 {
		suffix = 0
	}
	return prefix, suffix
}

// IsSurgical returns true if the hunk list contains at least one HunkKeep,
// meaning the narrowed path provides benefit over monolithic delete+retype.
func IsSurgical(hunks []Hunk) bool {
	for _, h := range hunks {
		if h.Op == HunkKeep {
			return true
		}
	}
	return false
}
