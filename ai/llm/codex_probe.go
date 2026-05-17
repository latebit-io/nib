package llm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
)

// logPrefixProbe emits a debug-level fingerprint of a Codex request's
// cache-relevant prefix so consecutive turns can be diffed. The
// fingerprint is three short SHA-256 prefixes plus a 64-char head of
// each section — short enough that the log stays scannable, specific
// enough that two turns with an identical prefix produce identical
// fingerprints. Any change in fingerprint between consecutive turns
// indicates the prefix moved and the prompt cache had to start over.
//
// Sections probed:
//   - instructions (system prompt) — busts the entire cache when changed.
//   - tools (JSON-marshaled in slice order) — busts the cache when
//     a tool is added/removed/renamed/reordered.
//   - first input item — the historical anchor. If a compaction or
//     mid-prefix insertion rewrites the start of input, this hash
//     changes and every subsequent turn pays full freight until the
//     new prefix re-populates.
//
// Diagnostic-only — keep at slog.Debug. Remove or downgrade once cache
// stability is solved.
func logPrefixProbe(model, instructions string, input []any, tools []ToolDef) {
	instrHash, instrHead := fingerprint(instructions)

	toolsHash := ""
	if len(tools) > 0 {
		if b, err := json.Marshal(tools); err == nil {
			toolsHash = shortHash(b)
		}
	}

	firstInputHash := ""
	firstInputHead := ""
	if len(input) > 0 {
		if b, err := json.Marshal(input[0]); err == nil {
			firstInputHash = shortHash(b)
			firstInputHead = head(string(b), 96)
		}
	}

	slog.Debug("codex prefix probe",
		"model", model,
		"instr_hash", instrHash,
		"instr_head", instrHead,
		"tools_hash", toolsHash,
		"tools_count", len(tools),
		"first_input_hash", firstInputHash,
		"first_input_head", firstInputHead,
		"input_items", len(input),
	)
}

// fingerprint returns a short hash and a 64-char head of s. Inlined
// rather than two helpers because the caller always wants both together.
func fingerprint(s string) (hash, hd string) {
	return shortHash([]byte(s)), head(s, 64)
}

// shortHash returns the first 8 hex bytes (16 chars) of SHA-256(b).
// Short enough to keep logs tight; long enough that a collision
// between two distinct prefixes is practically zero.
func shortHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// head returns the first n characters of s with newlines collapsed to
// spaces so the value renders as a single key=value pair in slog text
// output. Truncates with an ellipsis when shorter than the full string.
func head(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
