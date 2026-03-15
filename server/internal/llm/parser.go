package llm

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/latebit-io/junto/protocol"
)

// Parser is a stateful streaming parser that separates reasoning text
// from fenced ```op blocks containing EditOp JSON.
// It also strips <think>...</think> blocks that some LLMs emit for chain-of-thought.
type Parser struct {
	lineBuf  strings.Builder // accumulates the current line
	fenceBuf strings.Builder // accumulates content inside a fence
	inFence  bool
	inThink  bool // inside <think>...</think> block
}

// Feed processes a token from the LLM stream.
// Returns reasoning text to display and any EditOps parsed from the token.
// reasoning may be empty. ops may be empty or contain multiple ops if the LLM
// emitted several fenced blocks in one chunk.
func (p *Parser) Feed(token string) (reasoning string, ops []*protocol.EditOp) {
	for i := 0; i < len(token); {
		nl := strings.IndexByte(token[i:], '\n')
		var segment string
		if nl == -1 {
			segment = token[i:]
			i = len(token)
		} else {
			segment = token[i : i+nl+1]
			i = i + nl + 1
		}

		p.lineBuf.WriteString(segment)

		if !strings.HasSuffix(segment, "\n") {
			continue
		}

		// Complete line ready
		line := p.lineBuf.String()
		trimmed := strings.TrimSpace(line)
		p.lineBuf.Reset()

		if !p.inFence {
			if trimmed == "```op" {
				p.fenceBuf.Reset()
				p.inFence = true
				continue
			}
			// Strip <think>...</think> blocks from reasoning output
			line = StripThinkTags(line, &p.inThink)
			if line != "" {
				reasoning += line
			}
		} else {
			if trimmed == "```" {
				content := strings.TrimSpace(p.fenceBuf.String())
				p.fenceBuf.Reset()
				p.inFence = false

				if op := parseOp(content); op != nil {
					ops = append(ops, op)
				}
				continue
			}
			p.fenceBuf.WriteString(line)
		}
	}

	return reasoning, ops
}

// FlushOp checks if the lineBuf holds an unterminated closing fence and
// returns any final op. Call this after the stream ends.
func (p *Parser) FlushOp() *protocol.EditOp {
	if !p.inFence {
		return nil
	}
	remaining := strings.TrimSpace(p.lineBuf.String())
	if remaining != "```" {
		return nil
	}
	p.lineBuf.Reset()
	content := strings.TrimSpace(p.fenceBuf.String())
	p.fenceBuf.Reset()
	p.inFence = false

	return parseOp(content)
}

// Flush returns any remaining buffered text as reasoning.
// Call this when the stream ends.
func (p *Parser) Flush() string {
	out := StripThinkTags(p.lineBuf.String(), &p.inThink)
	p.lineBuf.Reset()
	p.fenceBuf.Reset()
	p.inFence = false
	return out
}

// StripThinkTags removes <think>...</think> content from a string.
// inThink tracks state across calls for multi-line think blocks.
func StripThinkTags(s string, inThink *bool) string {
	var out strings.Builder
	for len(s) > 0 {
		if *inThink {
			end := strings.Index(s, "</think>")
			if end == -1 {
				return out.String() // entire remainder is inside think
			}
			s = s[end+len("</think>"):]
			*inThink = false
		} else {
			start := strings.Index(s, "<think>")
			if start == -1 {
				out.WriteString(s)
				return out.String()
			}
			out.WriteString(s[:start])
			s = s[start+len("<think>"):]
			*inThink = true
		}
	}
	return out.String()
}

// parseOp tries to decode an EditOp from fence content.
// Uses json.Decoder which tolerates trailing garbage (stray braces, whitespace)
// that LLMs sometimes append after the JSON object.
func parseOp(content string) *protocol.EditOp {
	var editOp protocol.EditOp
	dec := json.NewDecoder(strings.NewReader(content))
	if err := dec.Decode(&editOp); err != nil {
		slog.Warn("failed to parse op block", "err", err, "content", content)
		return nil
	}
	if editOp.ID == "" {
		slog.Warn("op block missing id", "content", content)
		return nil
	}
	if editOp.Search == "" {
		slog.Warn("op block missing search", "content", content)
		return nil
	}
	return &editOp
}
