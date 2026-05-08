package command

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	kitcmd "github.com/latebit-io/nib/kit/command"
)

// fakeLookup implements [CommandLookup]. nil means "no command
// registered"; populated map means the listed names exist.
type fakeLookup struct {
	registered map[string]kitcmd.Command
}

func (f *fakeLookup) Lookup(name string) (kitcmd.Command, bool) {
	c, ok := f.registered[name]
	return c, ok
}

// stubCommand is a minimal kitcmd.Command for fake-registry entries.
type stubCommand struct{ def kitcmd.Definition }

func (s *stubCommand) Definition() kitcmd.Definition { return s.def }

func TestNewCommand_CreatesFileWithScaffold(t *testing.T) {
	dir := t.TempDir()
	cmd := NewNewCommand(dir, &fakeLookup{})
	sess := &recordingSession{}
	if err := cmd.Handle(context.Background(), sess, "review"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	path := filepath.Join(dir, "review.md")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasPrefix(string(got), "---") {
		t.Errorf("scaffold should open with frontmatter fence; got %q", got)
	}
	if !strings.Contains(string(got), "TODO") {
		t.Errorf("scaffold should signal TODO content; got %q", got)
	}
	if len(sess.displays) != 1 || !strings.Contains(sess.displays[0], path) {
		t.Errorf("Display should mention the created path; got %v", sess.displays)
	}
}

func TestNewCommand_LowercasesName(t *testing.T) {
	// /new-command Review should produce review.md (lowercased on
	// disk) so the loader's name-from-filename fallback agrees with
	// the registry's case-insensitive lookup.
	dir := t.TempDir()
	cmd := NewNewCommand(dir, &fakeLookup{})
	sess := &recordingSession{}
	if err := cmd.Handle(context.Background(), sess, "REVIEW"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "review.md")); err != nil {
		t.Errorf("expected review.md (lowercased); got %v", err)
	}
}

func TestNewCommand_RefusesEmptyName(t *testing.T) {
	cmd := NewNewCommand(t.TempDir(), &fakeLookup{})
	err := cmd.Handle(context.Background(), &recordingSession{}, "")
	if err == nil || !strings.Contains(err.Error(), "requires a name") {
		t.Errorf("expected 'requires a name' error; got %v", err)
	}
}

func TestNewCommand_RefusesInvalidName(t *testing.T) {
	// Names containing whitespace are caught earlier by the
	// one-argument check; tested separately above. These cases
	// reach the ValidName gate.
	cmd := NewNewCommand(t.TempDir(), &fakeLookup{})
	cases := []string{"has.dot", "slash/path", "questionmark?"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			err := cmd.Handle(context.Background(), &recordingSession{}, name)
			if err == nil || !strings.Contains(err.Error(), "invalid command name") {
				t.Errorf("name %q should be rejected; got %v", name, err)
			}
		})
	}
}

func TestNewCommand_RefusesMultipleArgs(t *testing.T) {
	// /new-command takes a single positional name. Trailing tokens
	// are a likely user error (typing a description inline) — fail
	// loudly so the user knows to write the description in the file.
	cmd := NewNewCommand(t.TempDir(), &fakeLookup{})
	err := cmd.Handle(context.Background(), &recordingSession{}, "review code-quality")
	if err == nil || !strings.Contains(err.Error(), "one argument") {
		t.Errorf("expected one-argument error; got %v", err)
	}
}

func TestNewCommand_RefusesShadowingExistingCommand(t *testing.T) {
	dir := t.TempDir()
	lookup := &fakeLookup{
		registered: map[string]kitcmd.Command{
			"compact": &stubCommand{
				def: kitcmd.Definition{
					Name:   "compact",
					Source: kitcmd.Source{Kind: kitcmd.SourceBuiltin, Path: "coding/command"},
				},
			},
		},
	}
	cmd := NewNewCommand(dir, lookup)
	err := cmd.Handle(context.Background(), &recordingSession{}, "compact")
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Errorf("expected already-registered error; got %v", err)
	}
	// And nothing should have been written.
	if _, statErr := os.Stat(filepath.Join(dir, "compact.md")); statErr == nil {
		t.Errorf("file should NOT have been written when name is shadowed")
	}
}

func TestNewCommand_RefusesOverwritingExistingFile(t *testing.T) {
	// File exists on disk but isn't (yet) registered — perhaps the
	// user is editing it and hasn't restarted. Don't clobber their
	// in-progress work.
	dir := t.TempDir()
	target := filepath.Join(dir, "review.md")
	if err := os.WriteFile(target, []byte("user edits in progress"), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cmd := NewNewCommand(dir, &fakeLookup{})
	err := cmd.Handle(context.Background(), &recordingSession{}, "review")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected file-already-exists error; got %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "user edits in progress" {
		t.Errorf("existing file was clobbered; got %q", got)
	}
}

func TestNewCommand_CreatesMissingParentDir(t *testing.T) {
	// commandDir might not exist if the user opted out of auto-seed
	// (e.g. mkdir + rmdir). /new-command creates it on demand.
	root := t.TempDir()
	dir := filepath.Join(root, ".project", "commands")
	cmd := NewNewCommand(dir, &fakeLookup{})
	if err := cmd.Handle(context.Background(), &recordingSession{}, "review"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "review.md")); err != nil {
		t.Errorf("expected file under created dir; got %v", err)
	}
}

func TestNewNewCommand_NilArgsPanic(t *testing.T) {
	cases := []struct {
		name string
		dir  string
		look CommandLookup
	}{
		{"empty dir", "", &fakeLookup{}},
		{"nil lookup", "/tmp/x", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("expected panic for %s", tc.name)
				}
			}()
			_ = NewNewCommand(tc.dir, tc.look)
		})
	}
}

func TestNewCommand_DispatchableThroughRegistry(t *testing.T) {
	dir := t.TempDir()
	r := kitcmd.NewRegistry()
	cmd := NewNewCommand(dir, r)
	if err := r.Register(cmd); err != nil {
		t.Fatalf("Register: %v", err)
	}
	sess := &recordingSession{}
	matched, err := r.Dispatch(context.Background(), sess, "/new-command review")
	if !matched || err != nil {
		t.Fatalf("Dispatch matched=%v err=%v", matched, err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "review.md")); statErr != nil {
		t.Errorf("expected review.md to exist after dispatch; got %v", statErr)
	}
}

func TestNewCommand_DefinitionShape(t *testing.T) {
	cmd := NewNewCommand(t.TempDir(), &fakeLookup{})
	def := cmd.Definition()
	if def.Name != "new-command" {
		t.Errorf("Name = %q, want new-command", def.Name)
	}
	wantAliases := map[string]bool{"newcmd": true}
	if len(def.Aliases) != len(wantAliases) {
		t.Errorf("Aliases = %v", def.Aliases)
	}
	for _, a := range def.Aliases {
		if !wantAliases[a] {
			t.Errorf("unexpected alias %q", a)
		}
	}
	if def.Source.Kind != kitcmd.SourceBuiltin {
		t.Errorf("Source.Kind = %v, want SourceBuiltin", def.Source.Kind)
	}
}
