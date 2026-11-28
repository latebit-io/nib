package main

import (
	"context"
	"testing"

	"github.com/latebit-io/nib/ai/llm"
	"github.com/latebit-io/nib/ai/llmtest"
	"github.com/latebit-io/nib/kit/nib"
)

func TestServeRun(t *testing.T) {
	assembly, err := nib.Assemble(context.Background(), nib.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeAssembly(assembly) })
	provider := llmtest.NewScripted(llmtest.Text("the answer"))
	history := []llm.Message{{Role: "user", Content: "earlier"}, {Role: "assistant", Content: "noted"}, {Role: "user", Content: "now"}}
	got, err := serveRun(assembly, provider)(context.Background(), history)
	if err != nil || got != "the answer" {
		t.Fatalf("run = %q, %v", got, err)
	}
	reqs := provider.Requests()
	if len(reqs) != 1 || len(reqs[0].Messages) != 4 || reqs[0].Messages[0].Role != "system" || reqs[0].Messages[3].Content != "now" {
		t.Errorf("request = %+v", reqs)
	}
}
