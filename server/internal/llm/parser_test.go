package llm

import (
	"testing"
)

func TestParserReasoningOnly(t *testing.T) {
	p := &Parser{}
	r, op := p.Feed("Hello world\n")
	if op != nil {
		t.Fatal("expected no op")
	}
	if r != "Hello world\n" {
		t.Fatalf("expected reasoning %q, got %q", "Hello world\n", r)
	}
}

func TestParserOpBlock(t *testing.T) {
	p := &Parser{}

	// Feed reasoning
	r1, op1 := p.Feed("Let me add a function.\n")
	if op1 != nil {
		t.Fatal("expected no op from reasoning")
	}
	if r1 != "Let me add a function.\n" {
		t.Fatalf("unexpected reasoning: %q", r1)
	}

	// Feed opening fence
	r2, op2 := p.Feed("```op\n")
	if op2 != nil {
		t.Fatal("expected no op from fence open")
	}
	if r2 != "" {
		t.Fatalf("expected no reasoning from fence open, got %q", r2)
	}

	// Feed JSON content
	r3, op3 := p.Feed(`{"id":"step-1","kind":"insert","line":3,"col":1,"text":"func foo() {}\n","reason":"add foo"}` + "\n")
	if op3 != nil {
		t.Fatal("expected no op yet (fence not closed)")
	}
	if r3 != "" {
		t.Fatalf("expected no reasoning inside fence, got %q", r3)
	}

	// Feed closing fence
	r4, op4 := p.Feed("```\n")
	if op4 == nil {
		t.Fatal("expected op from closing fence")
	}
	if op4.ID != "step-1" {
		t.Fatalf("expected op id step-1, got %s", op4.ID)
	}
	if op4.Kind != "insert" {
		t.Fatalf("expected kind insert, got %s", op4.Kind)
	}
	if op4.Line != 3 {
		t.Fatalf("expected line 3, got %d", op4.Line)
	}
	if op4.Text != "func foo() {}\n" {
		t.Fatalf("expected text %q, got %q", "func foo() {}\n", op4.Text)
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
	p.Feed(`{"id":"step-1","kind":"insert","line":1,"col":1,"text":"a\n","reason":"first"}` + "\n")
	_, op1 := p.Feed("```\n")
	if op1 == nil || op1.ID != "step-1" {
		t.Fatal("expected step-1")
	}

	// Reasoning between ops
	r, _ := p.Feed("Now step 2\n")
	if r != "Now step 2\n" {
		t.Fatalf("expected reasoning, got %q", r)
	}

	// Second op
	p.Feed("```op\n")
	p.Feed(`{"id":"step-2","kind":"insert","line":5,"col":1,"text":"b\n","reason":"second"}` + "\n")
	_, op2 := p.Feed("```\n")
	if op2 == nil || op2.ID != "step-2" {
		t.Fatal("expected step-2")
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
	_, op := p.Feed("```\n")
	if op != nil {
		t.Fatal("expected nil op for invalid JSON")
	}
}
