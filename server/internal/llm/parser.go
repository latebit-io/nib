package llm

import (
	"encoding/json"
	"strings"

	"github.com/latebit-io/junto/protocol"
)

// Parser is a stateful streaming parser that separates reasoning text
// from fenced ```op blocks containing EditOp JSON.
type Parser struct {
	lineBuf  strings.Builder // accumulates the current line
	fenceBuf strings.Builder // accumulates content inside a fence
	inFence  bool
}

// Feed processes a token from the LLM stream.
// Returns reasoning text to display and an EditOp if a complete op block was parsed.
// reasoning may be empty. op is nil unless a full ```op block just closed.
func (p *Parser) Feed(token string) (reasoning string, op *protocol.EditOp) {
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
			reasoning += line
		} else {
			if trimmed == "```" {
				content := strings.TrimSpace(p.fenceBuf.String())
				p.fenceBuf.Reset()
				p.inFence = false

				var editOp protocol.EditOp
				if err := json.Unmarshal([]byte(content), &editOp); err == nil && editOp.ID != "" {
					op = &editOp
				}
				continue
			}
			p.fenceBuf.WriteString(line)
		}
	}

	return reasoning, op
}

// Flush returns any remaining buffered text as reasoning.
// Call this when the stream ends.
func (p *Parser) Flush() string {
	out := p.lineBuf.String()
	p.lineBuf.Reset()
	p.fenceBuf.Reset()
	p.inFence = false
	return out
}
