package ui

import (
	"strings"
	"testing"

	"github.com/latebit-io/junto/engine/buffer"
	"github.com/latebit-io/junto/engine/editor"
	"github.com/latebit-io/junto/engine/lang"
)

// newTestEditorModel creates a minimal EditorModel for testing.
func newTestEditorModel(content string) *EditorModel {
	buf := buffer.NewFromString(content)
	eng := editor.New(buf)
	eng.SetSize(80, 24)
	return NewEditorModel(eng, DefaultKeymap(), NewServices())
}

func TestDiagnosticForLine(t *testing.T) {
	m := newTestEditorModel("line0\nline1\nline2\nline3\n")

	t.Run("nil when no diagnostics", func(t *testing.T) {
		m.SetDiagnostics(nil)
		if d := m.diagnosticForLine(0); d != nil {
			t.Errorf("expected nil, got %+v", d)
		}
	})

	t.Run("returns diagnostic on exact line", func(t *testing.T) {
		m.SetDiagnostics([]lang.Diagnostic{
			{StartLine: 1, EndLine: 1, Severity: lang.SeverityError, Message: "err"},
		})
		d := m.diagnosticForLine(1)
		if d == nil {
			t.Fatal("expected diagnostic, got nil")
		}
		if d.Message != "err" {
			t.Errorf("message = %q, want %q", d.Message, "err")
		}
	})

	t.Run("returns nil for unaffected line", func(t *testing.T) {
		m.SetDiagnostics([]lang.Diagnostic{
			{StartLine: 1, EndLine: 1, Severity: lang.SeverityError, Message: "err"},
		})
		if d := m.diagnosticForLine(0); d != nil {
			t.Errorf("expected nil for line 0, got %+v", d)
		}
	})

	t.Run("returns diagnostic for multi-line range", func(t *testing.T) {
		m.SetDiagnostics([]lang.Diagnostic{
			{StartLine: 0, EndLine: 2, Severity: lang.SeverityWarning, Message: "span"},
		})
		for _, line := range []int{0, 1, 2} {
			if d := m.diagnosticForLine(line); d == nil {
				t.Errorf("expected diagnostic on line %d", line)
			}
		}
		if d := m.diagnosticForLine(3); d != nil {
			t.Errorf("expected nil for line 3, got %+v", d)
		}
	})

	t.Run("returns highest severity", func(t *testing.T) {
		m.SetDiagnostics([]lang.Diagnostic{
			{StartLine: 1, EndLine: 1, Severity: lang.SeverityWarning, Message: "warn"},
			{StartLine: 1, EndLine: 1, Severity: lang.SeverityError, Message: "err"},
			{StartLine: 1, EndLine: 1, Severity: lang.SeverityHint, Message: "hint"},
		})
		d := m.diagnosticForLine(1)
		if d == nil {
			t.Fatal("expected diagnostic, got nil")
		}
		if d.Severity != lang.SeverityError {
			t.Errorf("severity = %d, want %d (Error)", d.Severity, lang.SeverityError)
		}
		if d.Message != "err" {
			t.Errorf("message = %q, want %q", d.Message, "err")
		}
	})
}

func TestDiagnosticGutterIcon(t *testing.T) {
	tests := []struct {
		name     string
		severity lang.Severity
		icon     string
	}{
		{"error", lang.SeverityError, diagErrorIcon},
		{"warning", lang.SeverityWarning, diagWarningIcon},
		{"info", lang.SeverityInfo, diagInfoIcon},
		{"hint", lang.SeverityHint, diagInfoIcon},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestEditorModel("hello world\n")
			m.SetDiagnostics([]lang.Diagnostic{
				{StartLine: 0, EndLine: 0, StartCol: 0, EndCol: 5, Severity: tt.severity, Message: "test"},
			})
			output := m.Render()
			if !strings.Contains(output, tt.icon) {
				t.Errorf("render output missing gutter icon %q for severity %d", tt.icon, tt.severity)
			}
		})
	}
}

func TestDiagnosticStatusBarMessage(t *testing.T) {
	m := newTestEditorModel("hello world\nsecond line\n")
	m.SetDiagnostics([]lang.Diagnostic{
		{StartLine: 0, EndLine: 0, Severity: lang.SeverityError, Message: "undefined: foo"},
	})
	// Cursor is on line 0 by default — status bar rendered separately.
	bar := m.renderStatusBar(80)
	if !strings.Contains(bar, "error: undefined: foo") {
		t.Errorf("status bar missing diagnostic message, got: %s", bar)
	}

	// Move cursor to line 1 — should not show diagnostic.
	m.eng.CursorLine = 1
	m.eng.CursorCol = 0
	bar = m.renderStatusBar(80)
	if strings.Contains(bar, "undefined: foo") {
		t.Errorf("status bar should not show diagnostic for line 1")
	}
}

func TestDiagnosticStatusBarNotShownWithStatusMsg(t *testing.T) {
	m := newTestEditorModel("hello world\n")
	m.SetDiagnostics([]lang.Diagnostic{
		{StartLine: 0, EndLine: 0, Severity: lang.SeverityError, Message: "undefined: foo"},
	})
	m.StatusMsg = "Saved!"
	bar := m.renderStatusBar(80)
	if strings.Contains(bar, "undefined: foo") {
		t.Errorf("diagnostic should not show when StatusMsg is set")
	}
	if !strings.Contains(bar, "Saved!") {
		t.Errorf("StatusMsg should be shown")
	}
}

func TestDiagnosticRenderWithUnderlines(t *testing.T) {
	// Verify rendering succeeds with diagnostic underlines and that the
	// output differs from the no-diagnostic case. Lipgloss doesn't emit
	// ANSI escapes in test mode (no TTY), so we can't check raw sequences.
	m := newTestEditorModel("hello world\n")

	outputClean := m.Render()

	m.SetDiagnostics([]lang.Diagnostic{
		{StartLine: 0, EndLine: 0, StartCol: 0, EndCol: 5, Severity: lang.SeverityError, Message: "err"},
	})
	outputDiag := m.Render()

	// The output must differ because the gutter icon and status bar message change.
	if outputClean == outputDiag {
		t.Error("expected output to differ when diagnostics are present")
	}
}

func TestDiagnosticMultiLineDiagRange(t *testing.T) {
	m := newTestEditorModel("first line\nsecond line\nthird line\n")
	m.SetDiagnostics([]lang.Diagnostic{
		{StartLine: 0, EndLine: 1, StartCol: 6, EndCol: 6, Severity: lang.SeverityWarning, Message: "multi"},
	})
	// Should render without panic.
	output := m.Render()
	if !strings.Contains(output, diagWarningIcon) {
		t.Error("expected warning icon in gutter")
	}
}
