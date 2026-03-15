package protocol

import (
	"encoding/json"
	"fmt"
)

// Message type constants.
const (
	// Server → Plugin
	TypeToken     = "token"
	TypeStep      = "step"
	TypePendingOp = "pending_op"
	TypeApproved  = "approved"
	TypeRejected  = "rejected"
	TypeContext   = "context"
	TypeError     = "error"

	// Plugin → Server
	TypeApprove  = "approve"
	TypeReject   = "reject"
	TypeContinue = "continue"
	TypeRedirect = "redirect"
	TypeEdit     = "edit"
	TypeStart    = "start"
)

// --- Server → Plugin messages ---

type TokenMsg struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type StepMsg struct {
	Type        string `json:"type"`
	Index       int    `json:"index"`
	Total       int    `json:"total"`
	Description string `json:"description"`
}

type PendingOpMsg struct {
	Type string `json:"type"`
	Op   EditOp `json:"op"`
}

type ApprovedMsg struct {
	Type string `json:"type"`
	OpID string `json:"op_id"`
}

type RejectedMsg struct {
	Type string `json:"type"`
	OpID string `json:"op_id"`
}

type ContextMsg struct {
	Type  string     `json:"type"`
	File  string     `json:"file"`
	Step  int        `json:"step"`
	Total int        `json:"total"`
	State AgentState `json:"state"`
}

type ErrorMsg struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// --- Plugin → Server messages ---

type ApproveMsg struct {
	Type string `json:"type"`
	OpID string `json:"op_id"`
}

type RejectMsg struct {
	Type string `json:"type"`
	OpID string `json:"op_id"`
}

type ContinueMsg struct {
	Type string `json:"type"`
}

type RedirectMsg struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type EditMsg struct {
	Type    string `json:"type"`
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	EndLine int    `json:"end_line"`
	EndCol  int    `json:"end_col"`
	Text    string `json:"text"`
}

type StartMsg struct {
	Type    string `json:"type"`
	File    string `json:"file"`
	Content string `json:"content"`
	Goal    string `json:"goal"`
}

// Parse reads a JSON line and returns the typed message.
func Parse(data []byte) (any, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("parse message type: %w", err)
	}

	var msg any
	switch probe.Type {
	case TypeToken:
		msg = &TokenMsg{}
	case TypeStep:
		msg = &StepMsg{}
	case TypePendingOp:
		msg = &PendingOpMsg{}
	case TypeApproved:
		msg = &ApprovedMsg{}
	case TypeRejected:
		msg = &RejectedMsg{}
	case TypeContext:
		msg = &ContextMsg{}
	case TypeError:
		msg = &ErrorMsg{}
	case TypeApprove:
		msg = &ApproveMsg{}
	case TypeReject:
		msg = &RejectMsg{}
	case TypeContinue:
		msg = &ContinueMsg{}
	case TypeRedirect:
		msg = &RedirectMsg{}
	case TypeEdit:
		msg = &EditMsg{}
	case TypeStart:
		msg = &StartMsg{}
	default:
		return nil, fmt.Errorf("unknown message type: %q", probe.Type)
	}

	if err := json.Unmarshal(data, msg); err != nil {
		return nil, fmt.Errorf("parse %s message: %w", probe.Type, err)
	}
	return msg, nil
}

// Marshal serializes a message to JSON with a newline terminator.
func Marshal(msg any) ([]byte, error) {
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
