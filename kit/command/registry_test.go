package command

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// --- fakes ---------------------------------------------------------

type fakeHandler struct {
	def         Definition
	handleErr   error
	gotArgs     string
	gotSess     Session
	handleCalls int
}

func (f *fakeHandler) Definition() Definition { return f.def }
func (f *fakeHandler) Handle(_ context.Context, sess Session, args string) error {
	f.handleCalls++
	f.gotArgs = args
	f.gotSess = sess
	return f.handleErr
}

type fakePrompt struct {
	def       Definition
	rendered  string
	renderErr error
	gotArgs   string
	calls     int
}

func (f *fakePrompt) Definition() Definition { return f.def }
func (f *fakePrompt) Render(args string) (string, error) {
	f.calls++
	f.gotArgs = args
	return f.rendered, f.renderErr
}

// fakeBare implements only Command — used to test ErrInvalidCommandShape.
type fakeBare struct {
	def Definition
}

func (f *fakeBare) Definition() Definition { return f.def }

type fakeSession struct {
	mu        sync.Mutex
	displays  []string
	submits   []string
	submitErr error
}

func (s *fakeSession) Display(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.displays = append(s.displays, text)
}

func (s *fakeSession) SubmitPrompt(_ context.Context, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.submitErr != nil {
		return s.submitErr
	}
	s.submits = append(s.submits, text)
	return nil
}

func builtinDef(name string) Definition {
	return Definition{
		Name:        name,
		Description: "test",
		Source:      Source{Kind: SourceBuiltin, Path: "test"},
	}
}

func sourceDef(name string, kind SourceKind) Definition {
	return Definition{
		Name:        name,
		Description: "test",
		Source:      Source{Kind: kind, Path: "test"},
	}
}

// --- Register ------------------------------------------------------

func TestRegister_Nil(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(nil); err == nil {
		t.Fatalf("Register(nil) should error")
	}
}

func TestRegister_InvalidName(t *testing.T) {
	cases := []string{"", "foo!", "foo bar", "foo.bar"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			r := NewRegistry()
			if err := r.Register(&fakeHandler{def: builtinDef(name)}); err == nil {
				t.Fatalf("Register name=%q should error", name)
			}
		})
	}
}

func TestRegister_InvalidAlias(t *testing.T) {
	r := NewRegistry()
	def := builtinDef("foo")
	def.Aliases = []string{"ba!d"}
	if err := r.Register(&fakeHandler{def: def}); err == nil {
		t.Fatalf("Register with invalid alias should error")
	}
}

func TestRegister_LookupAndList(t *testing.T) {
	r := NewRegistry()
	a := &fakeHandler{def: builtinDef("alpha")}
	b := &fakeHandler{def: builtinDef("beta")}
	if err := r.Register(b); err != nil {
		t.Fatalf("register beta: %v", err)
	}
	if err := r.Register(a); err != nil {
		t.Fatalf("register alpha: %v", err)
	}
	got, ok := r.Lookup("alpha")
	if !ok || got != a {
		t.Fatalf("Lookup(alpha) = (%v, %v), want a, true", got, ok)
	}
	got, ok = r.Lookup("ALPHA") // case-insensitive
	if !ok || got != a {
		t.Fatalf("Lookup(ALPHA) = (%v, %v), want a, true", got, ok)
	}
	list := r.List()
	if len(list) != 2 {
		t.Fatalf("List() len = %d, want 2", len(list))
	}
	// Sorted by canonical name.
	if list[0].Definition().Name != "alpha" || list[1].Definition().Name != "beta" {
		t.Errorf("List() = %q, %q; want alpha, beta",
			list[0].Definition().Name, list[1].Definition().Name)
	}
}

func TestRegister_AliasResolves(t *testing.T) {
	r := NewRegistry()
	def := builtinDef("foo")
	def.Aliases = []string{"f", "fooo"}
	cmd := &fakeHandler{def: def}
	if err := r.Register(cmd); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, key := range []string{"foo", "f", "F", "fooo", "FOOO"} {
		got, ok := r.Lookup(key)
		if !ok {
			t.Errorf("Lookup(%q) not found", key)
			continue
		}
		if got != cmd {
			t.Errorf("Lookup(%q) returned wrong command", key)
		}
	}
}

func TestRegister_AliasMatchesCanonical(t *testing.T) {
	// Alias matching the command's own canonical name is silently
	// deduped — registration succeeds.
	r := NewRegistry()
	def := builtinDef("foo")
	def.Aliases = []string{"foo"}
	if err := r.Register(&fakeHandler{def: def}); err != nil {
		t.Fatalf("register: %v", err)
	}
}

func TestRegister_AliasCollidesWithCanonical(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeHandler{def: builtinDef("foo")}); err != nil {
		t.Fatalf("register foo: %v", err)
	}
	def := builtinDef("bar")
	def.Aliases = []string{"foo"}
	if err := r.Register(&fakeHandler{def: def}); err == nil {
		t.Fatalf("alias collision with canonical should error")
	}
}

func TestRegister_AliasCollidesWithAlias(t *testing.T) {
	r := NewRegistry()
	def1 := builtinDef("foo")
	def1.Aliases = []string{"x"}
	if err := r.Register(&fakeHandler{def: def1}); err != nil {
		t.Fatalf("register foo: %v", err)
	}
	def2 := builtinDef("bar")
	def2.Aliases = []string{"x"}
	if err := r.Register(&fakeHandler{def: def2}); err == nil {
		t.Fatalf("alias collision should error")
	}
}

func TestRegister_SameKindCollision(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeHandler{def: builtinDef("foo")}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	err := r.Register(&fakeHandler{def: builtinDef("foo")})
	if err == nil {
		t.Fatalf("same-kind collision should error")
	}
	if !errors.Is(err, ErrCollision) {
		t.Errorf("err = %v, want ErrCollision", err)
	}
}

func TestRegister_HigherKindReplaces(t *testing.T) {
	r := NewRegistry()
	bIn := &fakeHandler{def: sourceDef("foo", SourceBuiltin)}
	pj := &fakeHandler{def: sourceDef("foo", SourceProject)}
	if err := r.Register(bIn); err != nil {
		t.Fatalf("register builtin: %v", err)
	}
	if err := r.Register(pj); err != nil {
		t.Fatalf("register project should not error (replaces): %v", err)
	}
	got, _ := r.Lookup("foo")
	if got != pj {
		t.Errorf("Lookup after replace = %v, want project", got)
	}
}

func TestRegister_LowerKindShadowed(t *testing.T) {
	r := NewRegistry()
	pj := &fakeHandler{def: sourceDef("foo", SourceProject)}
	bIn := &fakeHandler{def: sourceDef("foo", SourceBuiltin)}
	if err := r.Register(pj); err != nil {
		t.Fatalf("register project: %v", err)
	}
	if err := r.Register(bIn); err != nil {
		t.Fatalf("register builtin (shadowed) should not error: %v", err)
	}
	got, _ := r.Lookup("foo")
	if got != pj {
		t.Errorf("Lookup after shadowed-builtin = %v, want project (unchanged)", got)
	}
}

func TestRegister_ReplaceClearsAliases(t *testing.T) {
	r := NewRegistry()
	gd := sourceDef("foo", SourceGlobal)
	gd.Aliases = []string{"f"}
	gCmd := &fakeHandler{def: gd}
	if err := r.Register(gCmd); err != nil {
		t.Fatalf("register global: %v", err)
	}
	pd := sourceDef("foo", SourceProject)
	pd.Aliases = []string{"p"}
	pCmd := &fakeHandler{def: pd}
	if err := r.Register(pCmd); err != nil {
		t.Fatalf("register project: %v", err)
	}
	// Old alias "f" must not resolve any longer.
	if _, ok := r.Lookup("f"); ok {
		t.Errorf("Lookup(f) should not resolve after global was replaced")
	}
	// New alias "p" must resolve to project command.
	got, ok := r.Lookup("p")
	if !ok || got != pCmd {
		t.Errorf("Lookup(p) = (%v, %v), want project, true", got, ok)
	}
}

// TestRegister_ReplaceAliasCollisionPreservesExisting guards against
// registry corruption when a higher-precedence replacement fails alias
// validation: the previously-registered entry must remain intact, not
// be deleted alongside the rejected replacement.
func TestRegister_ReplaceAliasCollisionPreservesExisting(t *testing.T) {
	r := NewRegistry()

	// A third command owns alias "x".
	otherDef := sourceDef("other", SourceBuiltin)
	otherDef.Aliases = []string{"x"}
	other := &fakeHandler{def: otherDef}
	if err := r.Register(other); err != nil {
		t.Fatalf("register other: %v", err)
	}

	// Existing global "foo" with alias "f".
	gd := sourceDef("foo", SourceGlobal)
	gd.Aliases = []string{"f"}
	gCmd := &fakeHandler{def: gd}
	if err := r.Register(gCmd); err != nil {
		t.Fatalf("register global: %v", err)
	}

	// Higher-precedence project replacement whose alias "x" collides
	// with the third command — must error.
	pd := sourceDef("foo", SourceProject)
	pd.Aliases = []string{"x"}
	pCmd := &fakeHandler{def: pd}
	if err := r.Register(pCmd); err == nil {
		t.Fatalf("register project with colliding alias should error")
	}

	// Existing global "foo" and its alias "f" must still resolve;
	// the failed replacement must not have deleted them.
	got, ok := r.Lookup("foo")
	if !ok || got != gCmd {
		t.Errorf("Lookup(foo) after failed replacement = (%v, %v), want global, true", got, ok)
	}
	got, ok = r.Lookup("f")
	if !ok || got != gCmd {
		t.Errorf("Lookup(f) after failed replacement = (%v, %v), want global, true", got, ok)
	}
	// Third command must still own "x".
	got, ok = r.Lookup("x")
	if !ok || got != other {
		t.Errorf("Lookup(x) after failed replacement = (%v, %v), want other, true", got, ok)
	}
}

// --- Dispatch ------------------------------------------------------

func TestDispatch_NotASlash(t *testing.T) {
	r := NewRegistry()
	matched, err := r.Dispatch(context.Background(), &fakeSession{}, "hello world")
	if matched || err != nil {
		t.Errorf("matched=%v, err=%v; want false, nil", matched, err)
	}
}

func TestDispatch_UnknownCommand(t *testing.T) {
	r := NewRegistry()
	matched, err := r.Dispatch(context.Background(), &fakeSession{}, "/nope")
	if !matched {
		t.Fatalf("matched=false; want true")
	}
	if !errors.Is(err, ErrUnknownCommand) {
		t.Errorf("err = %v, want ErrUnknownCommand", err)
	}
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Name != "nope" {
		t.Errorf("err not a *CommandError with Name=nope: %v", err)
	}
}

func TestDispatch_HandlerSuccess(t *testing.T) {
	r := NewRegistry()
	h := &fakeHandler{def: builtinDef("foo")}
	if err := r.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}
	sess := &fakeSession{}
	matched, err := r.Dispatch(context.Background(), sess, "/foo bar baz")
	if !matched || err != nil {
		t.Fatalf("matched=%v, err=%v", matched, err)
	}
	if h.handleCalls != 1 {
		t.Errorf("handleCalls = %d, want 1", h.handleCalls)
	}
	if h.gotArgs != "bar baz" {
		t.Errorf("args = %q, want %q", h.gotArgs, "bar baz")
	}
	if h.gotSess != sess {
		t.Errorf("sess threading mismatch")
	}
}

func TestDispatch_HandlerError(t *testing.T) {
	sentinel := errors.New("handler boom")
	r := NewRegistry()
	if err := r.Register(&fakeHandler{def: builtinDef("foo"), handleErr: sentinel}); err != nil {
		t.Fatalf("register: %v", err)
	}
	matched, err := r.Dispatch(context.Background(), &fakeSession{}, "/foo")
	if !matched {
		t.Fatalf("matched=false")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want wrapping %v", err, sentinel)
	}
}

func TestDispatch_PromptSuccess(t *testing.T) {
	r := NewRegistry()
	p := &fakePrompt{def: builtinDef("foo"), rendered: "rendered template: bar"}
	if err := r.Register(p); err != nil {
		t.Fatalf("register: %v", err)
	}
	sess := &fakeSession{}
	matched, err := r.Dispatch(context.Background(), sess, "/foo bar")
	if !matched || err != nil {
		t.Fatalf("matched=%v, err=%v", matched, err)
	}
	if p.calls != 1 || p.gotArgs != "bar" {
		t.Errorf("Render calls=%d args=%q; want 1, bar", p.calls, p.gotArgs)
	}
	if len(sess.submits) != 1 || sess.submits[0] != "rendered template: bar" {
		t.Errorf("submits = %v, want [rendered template: bar]", sess.submits)
	}
}

func TestDispatch_PromptRenderError(t *testing.T) {
	rerr := errors.New("render boom")
	r := NewRegistry()
	if err := r.Register(&fakePrompt{def: builtinDef("foo"), renderErr: rerr}); err != nil {
		t.Fatalf("register: %v", err)
	}
	matched, err := r.Dispatch(context.Background(), &fakeSession{}, "/foo")
	if !matched {
		t.Fatalf("matched=false")
	}
	if !errors.Is(err, rerr) {
		t.Errorf("err = %v, want wrapping %v", err, rerr)
	}
}

func TestDispatch_PromptSubmitError(t *testing.T) {
	serr := errors.New("submit boom")
	r := NewRegistry()
	if err := r.Register(&fakePrompt{def: builtinDef("foo"), rendered: "x"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	sess := &fakeSession{submitErr: serr}
	matched, err := r.Dispatch(context.Background(), sess, "/foo")
	if !matched {
		t.Fatalf("matched=false")
	}
	if !errors.Is(err, serr) {
		t.Errorf("err = %v, want wrapping %v", err, serr)
	}
}

func TestDispatch_TurnInFlight(t *testing.T) {
	r := NewRegistry()
	h := &fakeHandler{def: builtinDef("foo")}
	if err := r.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}
	matched, err := r.Dispatch(
		context.Background(), &fakeSession{}, "/foo",
		WithBusyCheck(func() bool { return true }),
	)
	if !matched {
		t.Fatalf("matched=false")
	}
	if !errors.Is(err, ErrTurnInFlight) {
		t.Errorf("err = %v, want ErrTurnInFlight", err)
	}
	if h.handleCalls != 0 {
		t.Errorf("handler should not run when busy; calls=%d", h.handleCalls)
	}
}

func TestDispatch_NotBusyDispatches(t *testing.T) {
	r := NewRegistry()
	h := &fakeHandler{def: builtinDef("foo")}
	if err := r.Register(h); err != nil {
		t.Fatalf("register: %v", err)
	}
	matched, err := r.Dispatch(
		context.Background(), &fakeSession{}, "/foo",
		WithBusyCheck(func() bool { return false }),
	)
	if !matched || err != nil || h.handleCalls != 1 {
		t.Errorf("matched=%v err=%v calls=%d", matched, err, h.handleCalls)
	}
}

func TestDispatch_UnknownTakesPrecedenceOverBusy(t *testing.T) {
	// An unknown command will never become known by retrying; report
	// ErrUnknownCommand even when busy check would fail.
	r := NewRegistry()
	matched, err := r.Dispatch(
		context.Background(), &fakeSession{}, "/nope",
		WithBusyCheck(func() bool { return true }),
	)
	if !matched {
		t.Fatalf("matched=false")
	}
	if !errors.Is(err, ErrUnknownCommand) {
		t.Errorf("err = %v, want ErrUnknownCommand (not ErrTurnInFlight)", err)
	}
}

func TestDispatch_InvalidShape(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeBare{def: builtinDef("foo")}); err != nil {
		t.Fatalf("register bare: %v", err)
	}
	matched, err := r.Dispatch(context.Background(), &fakeSession{}, "/foo")
	if !matched {
		t.Fatalf("matched=false")
	}
	if !errors.Is(err, ErrInvalidCommandShape) {
		t.Errorf("err = %v, want ErrInvalidCommandShape", err)
	}
}

// --- CommandError --------------------------------------------------

func TestCommandError_FormatAndUnwrap(t *testing.T) {
	cause := errors.New("inner")
	ce := &CommandError{Name: "foo", Err: cause}
	if !strings.Contains(ce.Error(), `"foo"`) {
		t.Errorf("Error() = %q, want to contain command name", ce.Error())
	}
	if !errors.Is(ce, cause) {
		t.Errorf("errors.Is should reach inner cause")
	}
	if got := ce.Unwrap(); got != cause {
		t.Errorf("Unwrap() = %v, want %v", got, cause)
	}
}
