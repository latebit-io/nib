package protocol

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantType string
		check   func(t *testing.T, msg any)
		wantErr bool
	}{
		{
			name:     "token message",
			input:    `{"type":"token","text":"hello world"}`,
			wantType: "*protocol.TokenMsg",
			check: func(t *testing.T, msg any) {
				t.Helper()
				m := msg.(*TokenMsg)
				if m.Text != "hello world" {
					t.Errorf("Text = %q, want %q", m.Text, "hello world")
				}
			},
		},
		{
			name:     "step message",
			input:    `{"type":"step","index":2,"total":5,"description":"write interface"}`,
			wantType: "*protocol.StepMsg",
			check: func(t *testing.T, msg any) {
				t.Helper()
				m := msg.(*StepMsg)
				if m.Index != 2 || m.Total != 5 || m.Description != "write interface" {
					t.Errorf("got %+v", m)
				}
			},
		},
		{
			name:     "pending_op message",
			input:    `{"type":"pending_op","op":{"id":"abc","kind":"insert","line":4,"col":1,"text":"hello\n","reason":"test"}}`,
			wantType: "*protocol.PendingOpMsg",
			check: func(t *testing.T, msg any) {
				t.Helper()
				m := msg.(*PendingOpMsg)
				if m.Op.ID != "abc" || m.Op.Kind != "insert" || m.Op.Line != 4 || m.Op.Col != 1 {
					t.Errorf("got %+v", m.Op)
				}
				if m.Op.Text != "hello\n" {
					t.Errorf("Text = %q, want %q", m.Op.Text, "hello\n")
				}
			},
		},
		{
			name:     "approved message",
			input:    `{"type":"approved","op_id":"abc123"}`,
			wantType: "*protocol.ApprovedMsg",
			check: func(t *testing.T, msg any) {
				t.Helper()
				if msg.(*ApprovedMsg).OpID != "abc123" {
					t.Errorf("OpID = %q", msg.(*ApprovedMsg).OpID)
				}
			},
		},
		{
			name:     "rejected message",
			input:    `{"type":"rejected","op_id":"abc123"}`,
			wantType: "*protocol.RejectedMsg",
		},
		{
			name:     "context message",
			input:    `{"type":"context","file":"main.go","step":1,"total":3,"state":"pending"}`,
			wantType: "*protocol.ContextMsg",
			check: func(t *testing.T, msg any) {
				t.Helper()
				m := msg.(*ContextMsg)
				if m.File != "main.go" || m.State != StatePending {
					t.Errorf("got %+v", m)
				}
			},
		},
		{
			name:     "error message",
			input:    `{"type":"error","message":"timeout"}`,
			wantType: "*protocol.ErrorMsg",
			check: func(t *testing.T, msg any) {
				t.Helper()
				if msg.(*ErrorMsg).Message != "timeout" {
					t.Errorf("Message = %q", msg.(*ErrorMsg).Message)
				}
			},
		},
		{
			name:     "approve message",
			input:    `{"type":"approve","op_id":"xyz"}`,
			wantType: "*protocol.ApproveMsg",
		},
		{
			name:     "reject message",
			input:    `{"type":"reject","op_id":"xyz"}`,
			wantType: "*protocol.RejectMsg",
		},
		{
			name:     "redirect message",
			input:    `{"type":"redirect","message":"add logging"}`,
			wantType: "*protocol.RedirectMsg",
			check: func(t *testing.T, msg any) {
				t.Helper()
				if msg.(*RedirectMsg).Message != "add logging" {
					t.Errorf("Message = %q", msg.(*RedirectMsg).Message)
				}
			},
		},
		{
			name:     "edit message",
			input:    `{"type":"edit","line":4,"col":1,"end_line":6,"end_col":1,"text":"new code"}`,
			wantType: "*protocol.EditMsg",
			check: func(t *testing.T, msg any) {
				t.Helper()
				m := msg.(*EditMsg)
				if m.Line != 4 || m.EndLine != 6 || m.Text != "new code" {
					t.Errorf("got %+v", m)
				}
			},
		},
		{
			name:     "start message",
			input:    `{"type":"start","file":"main.go","plan":[{"description":"step 1","done":false}]}`,
			wantType: "*protocol.StartMsg",
			check: func(t *testing.T, msg any) {
				t.Helper()
				m := msg.(*StartMsg)
				if m.File != "main.go" || len(m.Plan) != 1 || m.Plan[0].Description != "step 1" {
					t.Errorf("got %+v", m)
				}
			},
		},
		{
			name:    "unknown type",
			input:   `{"type":"bogus"}`,
			wantErr: true,
		},
		{
			name:    "invalid JSON",
			input:   `not json`,
			wantErr: true,
		},
		{
			name:    "missing type field",
			input:   `{"text":"no type"}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, err := Parse([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			gotType := typeName(msg)
			if gotType != tt.wantType {
				t.Errorf("type = %s, want %s", gotType, tt.wantType)
			}

			if tt.check != nil {
				tt.check(t, msg)
			}
		})
	}
}

func TestMarshal(t *testing.T) {
	tests := []struct {
		name string
		msg  any
	}{
		{"token", TokenMsg{Type: TypeToken, Text: "hello"}},
		{"approve", ApproveMsg{Type: TypeApprove, OpID: "abc"}},
		{"pending_op", PendingOpMsg{Type: TypePendingOp, Op: EditOp{
			ID: "x", Kind: "insert", Line: 1, Col: 1, Text: "code\n", Reason: "test",
		}}},
		{"start", StartMsg{Type: TypeStart, File: "f.go", Plan: []Step{{Description: "s1"}}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := Marshal(tt.msg)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			// Must end with newline.
			if len(data) == 0 || data[len(data)-1] != '\n' {
				t.Fatal("Marshal output must end with newline")
			}
			// Must be valid JSON (without the trailing newline).
			if !json.Valid(data[:len(data)-1]) {
				t.Fatalf("Marshal output is not valid JSON: %s", data)
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	original := PendingOpMsg{
		Type: TypePendingOp,
		Op: EditOp{
			ID:      "rt-1",
			Kind:    "replace",
			Line:    10,
			Col:     5,
			EndLine: 12,
			EndCol:  1,
			Text:    "func Verify() error {\n\treturn nil\n}\n",
			Reason:  "round trip test",
		},
	}

	data, err := Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Strip trailing newline for Parse (it expects raw JSON).
	parsed, err := Parse(data[:len(data)-1])
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	got, ok := parsed.(*PendingOpMsg)
	if !ok {
		t.Fatalf("expected *PendingOpMsg, got %T", parsed)
	}

	if got.Op.ID != original.Op.ID || got.Op.Kind != original.Op.Kind ||
		got.Op.Line != original.Op.Line || got.Op.Text != original.Op.Text ||
		got.Op.Reason != original.Op.Reason {
		t.Errorf("round trip mismatch:\n  got:  %+v\n  want: %+v", got.Op, original.Op)
	}
}

func typeName(v any) string {
	return fmt.Sprintf("%T", v)
}
