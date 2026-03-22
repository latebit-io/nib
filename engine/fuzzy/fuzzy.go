// Package fuzzy provides scored fuzzy matching for filtering and ranking
// strings. It supports four matching strategies with decreasing strictness:
// exact, prefix, substring, and fuzzy (character-skip). Each strategy
// contributes to the final score, with bonuses for consecutive matches,
// matches after path separators, and camelCase boundaries.
//
// This is an engine-level package — no UI dependencies. Usable by any
// frontend (command palette, file finder) and by the agent (file search).
package fuzzy

import (
	"sort"
	"strings"
	"unicode"
)

// Match represents a scored match of a query against a candidate string.
type Match struct {
	// Text is the original candidate string.
	Text string
	// Score is the match quality (higher is better). Zero means no match.
	Score int
	// Positions contains the indices of matched runes in Text (for highlighting).
	Positions []int
}

// Score computes a match score for query against candidate.
// Returns a Match with score > 0 if the candidate matches, or score 0 if not.
// Both query and candidate are compared case-insensitively.
func Score(query, candidate string) Match {
	if query == "" {
		return Match{Text: candidate, Score: 1}
	}

	qLower := strings.ToLower(query)
	cLower := strings.ToLower(candidate)
	cRunes := []rune(candidate)
	qRunes := []rune(qLower)

	// Strategy 1: Exact match (case-insensitive).
	if qLower == cLower {
		positions := make([]int, len(cRunes))
		for i := range positions {
			positions[i] = i
		}
		return Match{Text: candidate, Score: 1000 + lengthBonus(cRunes), Positions: positions}
	}

	// Strategy 2: Prefix match.
	if strings.HasPrefix(cLower, qLower) {
		positions := make([]int, len(qRunes))
		for i := range positions {
			positions[i] = i
		}
		return Match{Text: candidate, Score: 800 + lengthBonus(cRunes), Positions: positions}
	}

	// Strategy 3: Substring match — search over rune slices to avoid
	// byte/rune offset mismatch on non-ASCII candidates.
	cLowerRunes := []rune(cLower)
	if idx := runeIndex(cLowerRunes, qRunes); idx >= 0 {
		positions := make([]int, len(qRunes))
		for i := range positions {
			positions[i] = idx + i
		}
		score := 600 + lengthBonus(cRunes)
		// Bonus for matching right after a separator.
		if idx > 0 && isSeparator(cRunes[idx-1]) {
			score += 50
		}
		return Match{Text: candidate, Score: score, Positions: positions}
	}

	// Strategy 4: Fuzzy match — characters appear in order with gaps.
	return fuzzyScore(qRunes, cRunes, cLowerRunes, candidate)
}

// Filter scores all candidates against the query and returns only matches
// (score > 0), sorted by descending score. Ties are broken by text length
// (shorter first) then alphabetically.
func Filter(query string, candidates []string) []Match {
	if query == "" {
		results := make([]Match, len(candidates))
		for i, c := range candidates {
			results[i] = Match{Text: c, Score: 1}
		}
		sort.Slice(results, func(i, j int) bool {
			if len(results[i].Text) != len(results[j].Text) {
				return len(results[i].Text) < len(results[j].Text)
			}
			return results[i].Text < results[j].Text
		})
		return results
	}

	var results []Match
	for _, c := range candidates {
		m := Score(query, c)
		if m.Score > 0 {
			results = append(results, m)
		}
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		if len(results[i].Text) != len(results[j].Text) {
			return len(results[i].Text) < len(results[j].Text)
		}
		return results[i].Text < results[j].Text
	})

	return results
}

// fuzzyScore implements the character-skip fuzzy matching with scoring.
// cLower is the precomputed lowercase rune slice of candidate.
func fuzzyScore(query, candidate, cLower []rune, originalText string) Match {
	qi := 0
	var positions []int
	score := 0
	prevMatchIdx := -1
	consecutiveBonus := 0

	for ci := 0; ci < len(cLower) && qi < len(query); ci++ {
		if cLower[ci] == query[qi] {
			positions = append(positions, ci)
			matchScore := 10 // base score per matched char

			// Consecutive match bonus — rewards runs of matching chars.
			if prevMatchIdx == ci-1 {
				consecutiveBonus++
				matchScore += consecutiveBonus * 5
			} else {
				consecutiveBonus = 0
			}

			// Separator bonus — match right after /, ., _, -.
			if ci > 0 && isSeparator(candidate[ci-1]) {
				matchScore += 30
			}

			// Start of string bonus.
			if ci == 0 {
				matchScore += 20
			}

			// CamelCase boundary bonus — lowercase followed by uppercase.
			if ci > 0 && unicode.IsLower(candidate[ci-1]) && unicode.IsUpper(candidate[ci]) {
				matchScore += 20
			}

			score += matchScore
			prevMatchIdx = ci
			qi++
		}
	}

	// All query chars must be matched.
	if qi < len(query) {
		return Match{Text: originalText, Score: 0}
	}

	score += lengthBonus(candidate)
	return Match{Text: originalText, Score: score, Positions: positions}
}

// lengthBonus gives shorter candidates a small advantage — less noise.
func lengthBonus(candidate []rune) int {
	return max(50-len(candidate), 0)
}

// isSeparator returns true for common path and word separators.
func isSeparator(r rune) bool {
	return r == '/' || r == '\\' || r == '.' || r == '_' || r == '-' || r == ' '
}

// runeIndex returns the index of the first occurrence of needle in haystack,
// or -1 if not found. Operates on rune slices to avoid byte/rune mismatches.
func runeIndex(haystack, needle []rune) int {
	if len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i <= len(haystack)-len(needle); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
