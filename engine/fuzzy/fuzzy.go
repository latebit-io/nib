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

// Strategy base scores — higher means stricter match.
const (
	scoreExact     = 1000
	scorePrefix    = 800
	scoreSubstring = 600
)

// Per-character bonuses in fuzzy matching.
const (
	bonusCharBase     = 10 // base score per matched character
	bonusConsecutive  = 5  // cumulative bonus per consecutive match
	bonusSeparator    = 30 // match right after /, ., _, -, \, space
	bonusStartOfStr   = 20 // match at position 0
	bonusCamelCase    = 20 // match at camelCase boundary
	bonusSepSubstring = 50 // substring match right after a separator
	bonusLengthMax    = 50 // max length bonus (shorter candidates preferred)
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
	qRunes := toLowerRunes([]rune(query))
	qLower := string(qRunes)
	return score(qLower, qRunes, candidate)
}

// score is the internal scorer that takes precomputed query data.
// Filter calls this directly to avoid recomputing per candidate.
func score(qLower string, qRunes []rune, candidate string) Match {
	cRunes := []rune(candidate)
	// Per-rune lowercasing preserves slice length — safe for indexing back
	// into cRunes even with Unicode special-casing (e.g., ß stays 1 rune).
	cLowerRunes := toLowerRunes(cRunes)
	cLower := string(cLowerRunes)

	// Strategy 1: Exact match (case-insensitive).
	if qLower == cLower {
		positions := make([]int, len(cRunes))
		for i := range positions {
			positions[i] = i
		}
		return Match{Text: candidate, Score: scoreExact + lengthBonus(cRunes), Positions: positions}
	}

	// Strategy 2: Prefix match.
	if strings.HasPrefix(cLower, qLower) {
		positions := make([]int, len(qRunes))
		for i := range positions {
			positions[i] = i
		}
		return Match{Text: candidate, Score: scorePrefix + lengthBonus(cRunes), Positions: positions}
	}

	// Strategy 3: Substring match.
	if idx := runeIndex(cLowerRunes, qRunes); idx >= 0 {
		positions := make([]int, len(qRunes))
		for i := range positions {
			positions[i] = idx + i
		}
		score := scoreSubstring + lengthBonus(cRunes)
		// Bonus for matching right after a separator.
		if idx > 0 && isSeparator(cRunes[idx-1]) {
			score += bonusSepSubstring
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
			ri, rj := len([]rune(results[i].Text)), len([]rune(results[j].Text))
			if ri != rj {
				return ri < rj
			}
			return results[i].Text < results[j].Text
		})
		return results
	}

	qRunes := toLowerRunes([]rune(query))
	qLower := string(qRunes)
	results := make([]Match, 0, len(candidates))
	for _, c := range candidates {
		m := score(qLower, qRunes, c)
		if m.Score > 0 {
			results = append(results, m)
		}
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		ri, rj := len([]rune(results[i].Text)), len([]rune(results[j].Text))
		if ri != rj {
			return ri < rj
		}
		return results[i].Text < results[j].Text
	})

	return results
}

// charBonus computes the position-based bonus for matching at idx in candidate.
func charBonus(candidate []rune, idx int) int {
	bonus := bonusCharBase

	// Separator bonus — match right after /, ., _, -.
	if idx > 0 && isSeparator(candidate[idx-1]) {
		bonus += bonusSeparator
	}

	// Start of string bonus.
	if idx == 0 {
		bonus += bonusStartOfStr
	}

	// CamelCase boundary bonus — lowercase followed by uppercase.
	if idx > 0 && unicode.IsLower(candidate[idx-1]) && unicode.IsUpper(candidate[idx]) {
		bonus += bonusCamelCase
	}

	return bonus
}

// fuzzyScore implements the character-skip fuzzy matching with scoring.
// Uses a lookahead strategy: for each query character, scans all feasible
// positions and picks the one with the highest position bonus (separator,
// camelCase, start-of-string). This avoids the greedy left-to-right problem
// where an early match shadows a better boundary match.
func fuzzyScore(query, candidate, cLower []rune, originalText string) Match {
	qi := 0
	ci := 0
	var positions []int
	totalScore := 0
	prevMatchIdx := -1
	consecutiveCount := 0

	for qi < len(query) && ci < len(cLower) {
		remaining := len(query) - qi
		bestIdx := -1
		bestBonus := -1

		// Scan forward for the best position to match query[qi].
		for idx := ci; idx < len(cLower); idx++ {
			if cLower[idx] != query[qi] {
				continue
			}
			// Ensure enough chars remain for the rest of the query.
			if len(cLower)-idx < remaining {
				break
			}
			bonus := charBonus(candidate, idx)
			if bonus > bestBonus {
				bestBonus = bonus
				bestIdx = idx
			}
		}

		if bestIdx == -1 {
			break
		}

		positions = append(positions, bestIdx)
		matchScore := bestBonus

		// Consecutive match bonus — rewards runs of matching chars.
		if prevMatchIdx == bestIdx-1 {
			consecutiveCount++
			matchScore += consecutiveCount * bonusConsecutive
		} else {
			consecutiveCount = 0
		}

		totalScore += matchScore
		prevMatchIdx = bestIdx
		qi++
		ci = bestIdx + 1
	}

	// All query chars must be matched.
	if qi < len(query) {
		return Match{Text: originalText, Score: 0}
	}

	totalScore += lengthBonus(candidate)
	return Match{Text: originalText, Score: totalScore, Positions: positions}
}

// lengthBonus gives shorter candidates a small advantage — less noise.
func lengthBonus(candidate []rune) int {
	return max(bonusLengthMax-len(candidate), 0)
}

// isSeparator returns true for common path and word separators.
func isSeparator(r rune) bool {
	return r == '/' || r == '\\' || r == '.' || r == '_' || r == '-' || r == ' '
}

// toLowerRunes lowercases each rune individually, preserving slice length.
// Unlike strings.ToLower, this cannot change the rune count (e.g., ß stays
// as one rune), keeping indices aligned with the original candidate.
func toLowerRunes(runes []rune) []rune {
	lower := make([]rune, len(runes))
	for i, r := range runes {
		lower[i] = unicode.ToLower(r)
	}
	return lower
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
