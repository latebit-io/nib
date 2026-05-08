package command

import (
	"context"
	"errors"
	"strings"
	"testing"

	kitcmd "github.com/latebit-io/nib/kit/command"
)

// fakeCompactor implements [Compactor] for tests.
type fakeCompactor struct {
	calls int
	err   error
}

func (f *fakeCompactor) Compact(_ context.Context) error {
	f.calls++
	return f.err
}

// recordingSession captures Display calls. Tests assert against
// displayed text without coupling to a full TUI session.
type recordingSession struct {
	displays []string
}

func (r *recordingSession) Display(text string) {
	r.displays = append(r.displays, text)
}

func (r *recordingSession) SubmitPrompt(_ context.Context, _ string) error {
	return errors.New("recordingSession: SubmitPrompt unexpected for /compact")
}

func TestCompact_SuccessDisplaysCompactedLine(t *testing.T) {
	fc := &fakeCompactor{}
	c := NewCompact(fc, nil, nil)
	sess := &recordingSession{}
	if err := c.Handle(context.Background(), sess, ""); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fc.calls != 1 {
		t.Errorf("Compact calls = %d, want 1", fc.calls)
	}
	if len(sess.displays) != 1 || !strings.Contains(sess.displays[0], "compacted") {
		t.Errorf("Display = %v, want one line mentioning compacted", sess.displays)
	}
}

func TestCompact_NothingToCompactIsInformational(t *testing.T) {
	// A "nothing to compact" return is rendered as a friendly chrome
	// line, not surfaced as a registry error. The user typed a real
	// command and got a real (informational) answer.
	sentinel := errors.New("nothing")
	fc := &fakeCompactor{err: sentinel}
	c := NewCompact(fc, sentinel, nil)
	sess := &recordingSession{}
	if err := c.Handle(context.Background(), sess, ""); err != nil {
		t.Fatalf("Handle returned err for informational case: %v", err)
	}
	if len(sess.displays) != 1 || !strings.Contains(sess.displays[0], "Nothing") {
		t.Errorf("Display = %v, want a 'Nothing' line", sess.displays)
	}
}

func TestCompact_NoConversationIsInformational(t *testing.T) {
	sentinel := errors.New("none")
	fc := &fakeCompactor{err: sentinel}
	c := NewCompact(fc, nil, sentinel)
	sess := &recordingSession{}
	if err := c.Handle(context.Background(), sess, ""); err != nil {
		t.Fatalf("Handle returned err for informational case: %v", err)
	}
	if len(sess.displays) != 1 || !strings.Contains(sess.displays[0], "No conversation") {
		t.Errorf("Display = %v, want a 'No conversation' line", sess.displays)
	}
}

func TestCompact_GenuineErrorPropagates(t *testing.T) {
	// Errors that don't match either sentinel are returned to the
	// registry so the caller sees the failure.
	want := errors.New("provider unreachable")
	fc := &fakeCompactor{err: want}
	c := NewCompact(fc, errors.New("nothing"), errors.New("none"))
	sess := &recordingSession{}
	err := c.Handle(context.Background(), sess, "")
	if !errors.Is(err, want) {
		t.Fatalf("Handle err = %v, want %v", err, want)
	}
	if len(sess.displays) != 0 {
		t.Errorf("Display = %v, want no display on genuine error", sess.displays)
	}
}

func TestCompact_DefinitionShape(t *testing.T) {
	c := NewCompact(&fakeCompactor{}, nil, nil)
	def := c.Definition()
	if def.Name != "compact" {
		t.Errorf("Name = %q, want compact", def.Name)
	}
	if def.Source.Kind != kitcmd.SourceBuiltin {
		t.Errorf("Source.Kind = %v, want SourceBuiltin", def.Source.Kind)
	}
	if !strings.Contains(def.Description, "Compact") {
		t.Errorf("Description = %q, want it to mention Compact", def.Description)
	}
}

func TestNewCompact_NilCompactorPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("NewCompact(nil) did not panic")
		}
	}()
	_ = NewCompact(nil, nil, nil)
}

func TestCompact_DispatchableThroughRegistry(t *testing.T) {
	fc := &fakeCompactor{}
	c := NewCompact(fc, nil, nil)
	r := kitcmd.NewRegistry()
	if err := r.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	sess := &recordingSession{}
	matched, err := r.Dispatch(context.Background(), sess, "/compact")
	if !matched || err != nil {
		t.Fatalf("Dispatch matched=%v err=%v", matched, err)
	}
	if fc.calls != 1 {
		t.Errorf("Compact calls via registry = %d, want 1", fc.calls)
	}
}

// Compile-time guarantee: recordingSession satisfies kit.Session so
// drift in the kit interface fails the build instead of failing the
// test at runtime.
var _ kitcmd.Session = (*recordingSession)(nil)
