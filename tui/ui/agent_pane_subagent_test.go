package ui

import (
	"strings"
	"testing"

	"github.com/latebit-io/nib/coding/event"
)

func TestAppendSubagentActivity_RendersNestedBlock(t *testing.T) {
	m := beatPane()
	m.AppendSubagentActivity(event.SubagentActivity{Name: "reviewer", Phase: event.SubagentStarted})
	m.AppendSubagentActivity(event.SubagentActivity{Name: "reviewer", Phase: event.SubagentTool, Detail: "read_file"})
	m.AppendSubagentActivity(event.SubagentActivity{Name: "reviewer", Phase: event.SubagentFinished, Success: true})

	blocks := m.beats[0].Blocks
	if len(blocks) == 0 {
		t.Fatal("no blocks appended for subagent activity")
	}
	for i, b := range blocks {
		if b.Kind != BlockSubagent {
			t.Errorf("block[%d].Kind = %d; want BlockSubagent", i, b.Kind)
		}
	}

	transcript := strings.Join(m.RawLines, "\n")
	for _, want := range []string{"subagent reviewer", "read_file", "✓"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("transcript missing %q:\n%s", want, transcript)
		}
	}
	// The tool line nests one level deeper than the start/finish header.
	if !strings.Contains(transcript, "      ↳ read_file") {
		t.Errorf("subagent tool activity not nested-indented:\n%s", transcript)
	}
}

func TestAppendSubagentActivity_FailureGlyph(t *testing.T) {
	m := beatPane()
	m.AppendSubagentActivity(event.SubagentActivity{Name: "x", Phase: event.SubagentFinished, Success: false})
	if !strings.Contains(strings.Join(m.RawLines, "\n"), "✗") {
		t.Errorf("a failed subagent must render ✗:\n%s", strings.Join(m.RawLines, "\n"))
	}
}

func TestAppendSubagentActivity_RendersSummaryPreview(t *testing.T) {
	m := beatPane()
	m.AppendSubagentActivity(event.SubagentActivity{
		Name: "rev", Phase: event.SubagentFinished, Success: false,
		Detail: "could not apply the patch\nsecond paragraph that must not render",
	})
	transcript := strings.Join(m.RawLines, "\n")
	if !strings.Contains(transcript, "could not apply the patch") {
		t.Errorf("finished summary not rendered:\n%s", transcript)
	}
	if strings.Contains(transcript, "second paragraph") {
		t.Errorf("only the first summary line should render:\n%s", transcript)
	}
}

func TestSummaryPreview(t *testing.T) {
	if got := summaryPreview(""); got != "" {
		t.Errorf("blank summary → %q, want empty", got)
	}
	if got := summaryPreview("\n\n  hello  \nmore"); got != "hello" {
		t.Errorf("first non-empty line = %q, want %q", got, "hello")
	}
	long := strings.Repeat("x", subagentSummaryWidth+40)
	got := summaryPreview(long)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("long summary not truncated with ellipsis: %q", got)
	}
	if len([]rune(got)) > subagentSummaryWidth {
		t.Errorf("preview exceeds cap: %d runes", len([]rune(got)))
	}
}

func TestAppendSubagentActivity_IgnoresUnknownPhase(t *testing.T) {
	m := beatPane()
	before := len(m.RawLines)
	m.AppendSubagentActivity(event.SubagentActivity{Name: "x", Phase: event.SubagentPhase(99)})
	if len(m.RawLines) != before {
		t.Errorf("unknown phase should append nothing; RawLines grew %d→%d", before, len(m.RawLines))
	}
}
