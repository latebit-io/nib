package command

import (
	"context"
	"strings"
	"testing"
)

func TestHelp_DefinitionStable(t *testing.T) {
	r := NewRegistry()
	h := NewHelp(r)
	d1 := h.Definition()
	d2 := h.Definition()
	if d1.Name != d2.Name || d1.Description != d2.Description || d1.Source != d2.Source {
		t.Errorf("Definition() not stable: %+v vs %+v", d1, d2)
	}
	if d1.Name != "help" {
		t.Errorf("Name = %q, want help", d1.Name)
	}
	if d1.Source.Kind != SourceBuiltin {
		t.Errorf("Source.Kind = %v, want SourceBuiltin", d1.Source.Kind)
	}
}

func TestHelp_EmptyRegistryListsItself(t *testing.T) {
	r := NewRegistry()
	h := NewHelp(r)
	if err := r.Register(h); err != nil {
		t.Fatalf("register help: %v", err)
	}
	sess := &fakeSession{}
	matched, err := r.Dispatch(context.Background(), sess, "/help")
	if !matched || err != nil {
		t.Fatalf("dispatch: matched=%v err=%v", matched, err)
	}
	if len(sess.displays) != 1 {
		t.Fatalf("displays = %d, want 1", len(sess.displays))
	}
	out := sess.displays[0]
	if !strings.Contains(out, "/help") {
		t.Errorf("output should contain /help, got %q", out)
	}
}

func TestHelp_SortsCommands(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeHandler{def: builtinDef("zebra")}); err != nil {
		t.Fatalf("register zebra: %v", err)
	}
	if err := r.Register(&fakeHandler{def: builtinDef("apple")}); err != nil {
		t.Fatalf("register apple: %v", err)
	}
	if err := r.Register(&fakeHandler{def: builtinDef("mango")}); err != nil {
		t.Fatalf("register mango: %v", err)
	}
	if err := r.Register(NewHelp(r)); err != nil {
		t.Fatalf("register help: %v", err)
	}
	sess := &fakeSession{}
	if _, err := r.Dispatch(context.Background(), sess, "/help"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	out := sess.displays[0]
	// All in alphabetical order.
	apple := strings.Index(out, "/apple")
	help := strings.Index(out, "/help")
	mango := strings.Index(out, "/mango")
	zebra := strings.Index(out, "/zebra")
	if apple < 0 || help < 0 || mango < 0 || zebra < 0 {
		t.Fatalf("missing command in output:\n%s", out)
	}
	if apple >= help || help >= mango || mango >= zebra {
		t.Errorf("commands not sorted; output:\n%s", out)
	}
}

func TestHelp_RendersSourceTags(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeHandler{def: sourceDef("alpha", SourceBuiltin)}); err != nil {
		t.Fatalf("register alpha: %v", err)
	}
	if err := r.Register(&fakeHandler{def: sourceDef("bravo", SourceGlobal)}); err != nil {
		t.Fatalf("register bravo: %v", err)
	}
	if err := r.Register(&fakeHandler{def: sourceDef("charlie", SourceProject)}); err != nil {
		t.Fatalf("register charlie: %v", err)
	}
	// /help isn't registered in this test — call Handle directly to
	// exercise the source-tag rendering without depending on dispatch.
	h := NewHelp(r).(*helpCommand)
	sess := &fakeSession{}
	if err := h.Handle(context.Background(), sess, ""); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	out := sess.displays[0]
	// Builtin should not have a source tag; others should.
	alphaLine := lineWith(out, "/alpha")
	bravoLine := lineWith(out, "/bravo")
	charlieLine := lineWith(out, "/charlie")
	if strings.Contains(alphaLine, "[") {
		t.Errorf("builtin alpha line has source tag: %q", alphaLine)
	}
	if !strings.Contains(bravoLine, "[global]") {
		t.Errorf("global bravo line missing tag: %q", bravoLine)
	}
	if !strings.Contains(charlieLine, "[project]") {
		t.Errorf("project charlie line missing tag: %q", charlieLine)
	}
}

func TestHelp_NoCommandsRegistered(t *testing.T) {
	// /help dispatched against a registry where /help is the only
	// command should still list /help.
	r := NewRegistry()
	if err := r.Register(NewHelp(r)); err != nil {
		t.Fatalf("register help: %v", err)
	}
	sess := &fakeSession{}
	if _, err := r.Dispatch(context.Background(), sess, "/help"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	out := sess.displays[0]
	if !strings.Contains(out, "/help") {
		t.Errorf("missing /help in output: %q", out)
	}
	if strings.Contains(out, "(none registered)") {
		t.Errorf("should not show 'none registered' when /help itself is registered: %q", out)
	}
}

func TestHelp_TrulyEmpty(t *testing.T) {
	// Help command holding a registry pointer where nothing — not even
	// itself — has been registered.
	r := NewRegistry()
	h := NewHelp(r).(*helpCommand)
	sess := &fakeSession{}
	if err := h.Handle(context.Background(), sess, ""); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(sess.displays[0], "(none registered)") {
		t.Errorf("output should show 'none registered'; got %q", sess.displays[0])
	}
}

// lineWith returns the line in s containing substring sub, or "" if absent.
func lineWith(s, sub string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	return ""
}
