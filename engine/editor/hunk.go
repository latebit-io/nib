package editor

// HunkOp describes what a hunk does during surgical animation.
type HunkOp int

const (
	// HunkKeep means these lines are identical in search and replace — skip.
	HunkKeep HunkOp = iota
	// HunkDelete means these lines exist in search but not replace — remove instantly.
	HunkDelete
	// HunkInsert means these lines exist in replace but not search — type char-by-char.
	HunkInsert
	// HunkModify means these lines differ — delete old, type new char-by-char.
	HunkModify
)

// Hunk is a contiguous group of lines with the same operation.
type Hunk struct {
	// Op is the operation to perform.
	Op HunkOp
	// SearchStart is the index into the search lines this hunk consumes from.
	SearchStart int
	// SearchCount is the number of search lines consumed (Keep, Delete, Modify).
	SearchCount int
	// ReplaceLines holds the new content for Insert and Modify hunks.
	ReplaceLines []string
}

// ComputeHunks computes a line-level diff between search and replace lines,
// returning a sequence of hunks that describe how to transform search into
// replace. Uses prefix/suffix matching to identify unchanged lines, then
// pairs the middle region by position.
//
// The algorithm is simple and correct for the common case: localized changes
// within a function where the agent rewrites a few lines. LCS is unnecessary
// at this scale (tens of lines).
func ComputeHunks(searchLines, replaceLines []string) []Hunk {
	sLen := len(searchLines)
	rLen := len(replaceLines)

	// Match prefix: identical lines from the top.
	prefix := 0
	for prefix < sLen && prefix < rLen && searchLines[prefix] == replaceLines[prefix] {
		prefix++
	}

	// Match suffix: identical lines from the bottom, not overlapping prefix.
	suffix := 0
	for suffix < sLen-prefix && suffix < rLen-prefix &&
		searchLines[sLen-1-suffix] == replaceLines[rLen-1-suffix] {
		suffix++
	}

	// Middle region: lines that differ.
	sMid := sLen - prefix - suffix // search lines in the middle
	rMid := rLen - prefix - suffix // replace lines in the middle

	var hunks []Hunk

	// Prefix keep hunk.
	if prefix > 0 {
		hunks = append(hunks, Hunk{
			Op:          HunkKeep,
			SearchStart: 0,
			SearchCount: prefix,
		})
	}

	// Middle region: pair by position.
	common := min(sMid, rMid)
	for i := range common {
		hunks = append(hunks, Hunk{
			Op:           HunkModify,
			SearchStart:  prefix + i,
			SearchCount:  1,
			ReplaceLines: []string{replaceLines[prefix+i]},
		})
	}

	// Extra search lines → delete.
	if sMid > common {
		hunks = append(hunks, Hunk{
			Op:          HunkDelete,
			SearchStart: prefix + common,
			SearchCount: sMid - common,
		})
	}

	// Extra replace lines → insert.
	if rMid > common {
		extra := make([]string, rMid-common)
		copy(extra, replaceLines[prefix+common:prefix+rMid])
		hunks = append(hunks, Hunk{
			Op:           HunkInsert,
			SearchStart:  prefix + sMid, // after all search lines consumed
			SearchCount:  0,
			ReplaceLines: extra,
		})
	}

	// Suffix keep hunk.
	if suffix > 0 {
		hunks = append(hunks, Hunk{
			Op:          HunkKeep,
			SearchStart: sLen - suffix,
			SearchCount: suffix,
		})
	}

	return hunks
}
