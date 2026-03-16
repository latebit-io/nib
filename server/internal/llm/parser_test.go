package llm

import (
	"testing"
)

func TestParserReasoningOnly(t *testing.T) {
	p := &Parser{}
	r, ops := p.Feed("Hello world\n")
	if len(ops) != 0 {
		t.Fatal("expected no ops")
	}
	if r != "Hello world\n" {
		t.Fatalf("expected reasoning %q, got %q", "Hello world\n", r)
	}
}

func TestParserOpBlock(t *testing.T) {
	p := &Parser{}

	// Feed reasoning
	r1, ops1 := p.Feed("Let me add a function.\n")
	if len(ops1) != 0 {
		t.Fatal("expected no op from reasoning")
	}
	if r1 != "Let me add a function.\n" {
		t.Fatalf("unexpected reasoning: %q", r1)
	}

	// Feed opening fence
	r2, ops2 := p.Feed("```op\n")
	if len(ops2) != 0 {
		t.Fatal("expected no op from fence open")
	}
	if r2 != "" {
		t.Fatalf("expected no reasoning from fence open, got %q", r2)
	}

	// Feed JSON content
	r3, ops3 := p.Feed(`{"id":"step-1","search":"old code","replace":"new code","reason":"refactor"}` + "\n")
	if len(ops3) != 0 {
		t.Fatal("expected no op yet (fence not closed)")
	}
	if r3 != "" {
		t.Fatalf("expected no reasoning inside fence, got %q", r3)
	}

	// Feed closing fence
	r4, ops4 := p.Feed("```\n")
	if len(ops4) != 1 {
		t.Fatalf("expected 1 op from closing fence, got %d", len(ops4))
	}
	op := ops4[0]
	if op.ID != "step-1" {
		t.Fatalf("expected op id step-1, got %s", op.ID)
	}
	if op.Search != "old code" {
		t.Fatalf("expected search %q, got %q", "old code", op.Search)
	}
	if op.Replace != "new code" {
		t.Fatalf("expected replace %q, got %q", "new code", op.Replace)
	}
	if r4 != "" {
		t.Fatalf("expected no reasoning from closing fence, got %q", r4)
	}
}

func TestParserMultipleOps(t *testing.T) {
	p := &Parser{}

	// First op
	p.Feed("Step 1\n")
	p.Feed("```op\n")
	p.Feed(`{"id":"step-1","search":"a","replace":"b","reason":"first"}` + "\n")
	_, ops1 := p.Feed("```\n")
	if len(ops1) != 1 || ops1[0].ID != "step-1" {
		t.Fatal("expected step-1")
	}

	// Reasoning between ops
	r, _ := p.Feed("Now step 2\n")
	if r != "Now step 2\n" {
		t.Fatalf("expected reasoning, got %q", r)
	}

	// Second op
	p.Feed("```op\n")
	p.Feed(`{"id":"step-2","search":"c","replace":"d","reason":"second"}` + "\n")
	_, ops2 := p.Feed("```\n")
	if len(ops2) != 1 || ops2[0].ID != "step-2" {
		t.Fatal("expected step-2")
	}
}

func TestParserMultipleOpsInOneToken(t *testing.T) {
	p := &Parser{}

	// Both ops arrive in a single token (as seen with MiniMax)
	token := "I'll do two changes.\n" +
		"```op\n" +
		`{"id":"step-1","search":"old line 1","replace":"new line 1","reason":"first"}` + "\n" +
		"```\n" +
		"\n" +
		"```op\n" +
		`{"id":"step-2","search":"old line 2","replace":"new line 2","reason":"second"}` + "\n" +
		"```\n"

	reasoning, ops := p.Feed(token)
	if reasoning != "I'll do two changes.\n\n" {
		t.Fatalf("unexpected reasoning: %q", reasoning)
	}
	if len(ops) != 2 {
		t.Fatalf("expected 2 ops, got %d", len(ops))
	}
	if ops[0].ID != "step-1" {
		t.Fatalf("expected step-1, got %s", ops[0].ID)
	}
	if ops[1].ID != "step-2" {
		t.Fatalf("expected step-2, got %s", ops[1].ID)
	}
}

func TestParserChunkedTokens(t *testing.T) {
	// Tokens can arrive as partial lines
	p := &Parser{}

	r1, _ := p.Feed("Hel")
	if r1 != "" {
		t.Fatalf("expected no output for partial line, got %q", r1)
	}

	r2, _ := p.Feed("lo\n")
	if r2 != "Hello\n" {
		t.Fatalf("expected assembled line, got %q", r2)
	}
}

func TestParserFlush(t *testing.T) {
	p := &Parser{}
	p.Feed("trailing text")
	out := p.Flush()
	if out != "trailing text" {
		t.Fatalf("expected flush output %q, got %q", "trailing text", out)
	}
}

func TestParserInvalidJSON(t *testing.T) {
	p := &Parser{}
	p.Feed("```op\n")
	p.Feed("not json\n")
	_, ops := p.Feed("```\n")
	if len(ops) != 0 {
		t.Fatal("expected no ops for invalid JSON")
	}
}

func TestParserTrailingGarbage(t *testing.T) {
	// LLM sometimes appends a stray } after the JSON object
	p := &Parser{}
	p.Feed("```op\n")
	p.Feed(`{"id":"step-1","search":"old","replace":"new","reason":"refactor"}` + "\n")
	p.Feed("}\n")
	_, ops := p.Feed("```\n")
	if len(ops) != 1 {
		t.Fatalf("expected 1 op despite trailing garbage, got %d", len(ops))
	}
	if ops[0].ID != "step-1" {
		t.Fatalf("expected step-1, got %s", ops[0].ID)
	}
}

func TestParserThinkTags(t *testing.T) {
	p := &Parser{}

	// Think block spanning multiple Feed calls
	r1, _ := p.Feed("<think>\nThe user wants\n")
	if r1 != "" {
		t.Fatalf("expected no reasoning inside think, got %q", r1)
	}

	r2, _ := p.Feed("to do something.\n</think>\n")
	if r2 != "\n" {
		t.Fatalf("expected only newline after think close, got %q", r2)
	}

	r3, _ := p.Feed("I'll help you.\n")
	if r3 != "I'll help you.\n" {
		t.Fatalf("expected reasoning after think, got %q", r3)
	}
}

func TestParserMissingSearch(t *testing.T) {
	p := &Parser{}
	p.Feed("```op\n")
	p.Feed(`{"id":"step-1","replace":"new code","reason":"test"}` + "\n")
	_, ops := p.Feed("```\n")
	if len(ops) != 0 {
		t.Fatal("expected no ops when search is missing")
	}
}
