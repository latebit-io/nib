package agent

import (
	"strings"
	"testing"
)

func TestBuildMessagesIncludesContextSet(t *testing.T) {
	contextFiles := []string{"src/auth.go", "src/handler.go"}
	msgs := buildMessages("main.go", "package main", "add tests", contextFiles)

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	user := msgs[1].Content
	if !strings.Contains(user, "Context Set") {
		t.Error("user message should contain context set section")
	}
	if !strings.Contains(user, "- src/auth.go") {
		t.Error("user message should list src/auth.go")
	}
	if !strings.Contains(user, "- src/handler.go") {
		t.Error("user message should list src/handler.go")
	}
}

func TestBuildMessagesNoContextSet(t *testing.T) {
	msgs := buildMessages("main.go", "package main", "add tests", nil)

	user := msgs[1].Content
	if strings.Contains(user, "Context Set") {
		t.Error("user message should not contain context set section when no context files")
	}
}

func TestBuildMessagesSystemPromptIncludesContextSetRules(t *testing.T) {
	msgs := buildMessages("main.go", "package main", "add tests", nil)

	system := msgs[0].Content
	if !strings.Contains(system, "Context Set") {
		t.Error("system prompt should include context set section")
	}
}
