package ui

import (
	"strings"
	"testing"

	"github.com/latebit-io/nib/engine/lang"
)

func completionTestItems() []lang.CompletionItem {
	return []lang.CompletionItem{
		{Label: "Println", Kind: lang.CompletionFunction, Detail: "func(a ...any)", InsertText: "Println"},
		{Label: "Printf", Kind: lang.CompletionFunction, Detail: "func(format string, a ...any)", InsertText: "Printf"},
		{Label: "Print", Kind: lang.CompletionFunction, Detail: "func(a ...any)", InsertText: "Print"},
	}
}

func TestCompletionPopupShowDismiss(t *testing.T) {
	var c CompletionPopup
	c.Show(completionTestItems(), 5, 10)

	if !c.Active {
		t.Fatal("popup should be active after Show")
	}
	if len(c.Items) != 3 {
		t.Errorf("expected 3 items, got %d", len(c.Items))
	}
	if c.Selected != 0 {
		t.Errorf("selected should be 0, got %d", c.Selected)
	}

	c.Dismiss()
	if c.Active {
		t.Fatal("popup should be inactive after Dismiss")
	}
	if c.Items != nil {
		t.Error("items should be nil after Dismiss")
	}
}

func TestCompletionPopupNavigation(t *testing.T) {
	var c CompletionPopup
	c.Show(completionTestItems(), 0, 0)

	// Down wraps around.
	c.SelectNext()
	if c.Selected != 1 {
		t.Errorf("expected 1, got %d", c.Selected)
	}
	c.SelectNext()
	c.SelectNext() // wraps to 0
	if c.Selected != 0 {
		t.Errorf("expected wrap to 0, got %d", c.Selected)
	}

	// Up wraps around.
	c.SelectPrev() // wraps to last
	if c.Selected != 2 {
		t.Errorf("expected wrap to 2, got %d", c.Selected)
	}
}

func TestCompletionPopupSelectedItem(t *testing.T) {
	var c CompletionPopup

	// Not active.
	if c.SelectedItem() != nil {
		t.Error("should return nil when not active")
	}

	c.Show(completionTestItems(), 0, 0)
	item := c.SelectedItem()
	if item == nil {
		t.Fatal("should return item when active")
	}
	if item.Label != "Println" {
		t.Errorf("expected Println, got %s", item.Label)
	}

	c.SelectNext()
	item = c.SelectedItem()
	if item == nil || item.Label != "Printf" {
		t.Errorf("expected Printf, got %v", item)
	}
}

func TestCompletionPopupRender(t *testing.T) {
	var c CompletionPopup
	c.Show(completionTestItems(), 0, 0)

	output := c.Render(60)
	if output == "" {
		t.Fatal("render should produce output")
	}
	if !strings.Contains(output, "Println") {
		t.Error("output should contain Println")
	}
	if !strings.Contains(output, "Printf") {
		t.Error("output should contain Printf")
	}
}

func TestCompletionPopupRenderEmpty(t *testing.T) {
	var c CompletionPopup
	if c.Render(60) != "" {
		t.Error("inactive popup should render empty")
	}

	c.Show(nil, 0, 0)
	if c.Render(60) != "" {
		t.Error("empty items should render empty")
	}
}

func TestCompletionKindIcon(t *testing.T) {
	tests := []struct {
		kind lang.CompletionKind
		want string
	}{
		{lang.CompletionFunction, "ƒ"},
		{lang.CompletionMethod, "m"},
		{lang.CompletionVariable, "v"},
		{lang.CompletionType, "T"},
		{lang.CompletionKeyword, "k"},
		{0, " "}, // unknown
	}
	for _, tt := range tests {
		got := completionKindIcon(tt.kind)
		if got != tt.want {
			t.Errorf("kind %d: got %q, want %q", tt.kind, got, tt.want)
		}
	}
}

func TestCompletionScrolling(t *testing.T) {
	// Create more items than maxCompletionVisible.
	items := make([]lang.CompletionItem, 15)
	for i := range items {
		items[i] = lang.CompletionItem{Label: string(rune('A' + i)), Kind: lang.CompletionFunction}
	}

	var c CompletionPopup
	c.Show(items, 0, 0)

	// Navigate past visible window.
	for i := 0; i < maxCompletionVisible+2; i++ {
		c.SelectNext()
	}

	// ScrollOffset should have advanced.
	if c.ScrollOffset == 0 {
		t.Error("scroll should have advanced")
	}

	// Selected item should be visible.
	if c.Selected < c.ScrollOffset || c.Selected >= c.ScrollOffset+maxCompletionVisible {
		t.Errorf("selected %d not in visible range [%d, %d)", c.Selected, c.ScrollOffset, c.ScrollOffset+maxCompletionVisible)
	}
}

func TestAcceptCompletion(t *testing.T) {
	t.Run("no partial typed", func(t *testing.T) {
		m := newTestEditorModel("fmt.\n")
		m.eng.CursorLine = 0
		m.eng.CursorCol = 4 // after "fmt."

		m.Completion.Show(completionTestItems(), 0, 4)
		m.acceptCompletion()

		line := m.eng.Buf.LineText(0)
		if line != "fmt.Println" {
			t.Errorf("expected 'fmt.Println', got %q", line)
		}
	})

	t.Run("partial identifier replaced", func(t *testing.T) {
		// User typed "fmt.P" then accepted "Println" — should get "fmt.Println" not "fmt.PPrintln".
		m := newTestEditorModel("fmt.P\n")
		m.eng.CursorLine = 0
		m.eng.CursorCol = 5 // after "fmt.P"

		m.Completion.Show(completionTestItems(), 0, 5)
		m.acceptCompletion()

		line := m.eng.Buf.LineText(0)
		if line != "fmt.Println" {
			t.Errorf("expected 'fmt.Println', got %q", line)
		}
	})

	t.Run("longer partial replaced", func(t *testing.T) {
		// User typed "fmt.Pri" then accepted "Println".
		m := newTestEditorModel("fmt.Pri\n")
		m.eng.CursorLine = 0
		m.eng.CursorCol = 7 // after "fmt.Pri"

		m.Completion.Show(completionTestItems(), 0, 7)
		m.acceptCompletion()

		line := m.eng.Buf.LineText(0)
		if line != "fmt.Println" {
			t.Errorf("expected 'fmt.Println', got %q", line)
		}
	})
}
