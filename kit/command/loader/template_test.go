package loader

import "testing"

func TestSubstitute_ArgumentsRaw(t *testing.T) {
	// $ARGUMENTS is the verbatim string — internal whitespace
	// must survive. This is the form templates use when they want
	// to forward user input untouched.
	got := Substitute("review this: $ARGUMENTS", "  hello  world  ")
	want := "review this:   hello  world  "
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSubstitute_AtSignNormalizes(t *testing.T) {
	// $@ joins tokens with single spaces — runs of whitespace
	// collapse, leading/trailing whitespace is stripped.
	got := Substitute("focus: $@", "  hello  world  ")
	want := "focus: hello world"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSubstitute_PositionalArgs(t *testing.T) {
	got := Substitute("$1 wrote $2 lines about $3", "fritz 47 slash-commands")
	want := "fritz wrote 47 lines about slash-commands"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSubstitute_PositionalBeyondRangeIsEmpty(t *testing.T) {
	// Templates referencing a missing arg get empty string, not an
	// error and not the literal "$N" — silent fallback keeps the
	// frontend's render path simple.
	got := Substitute("a=$1 b=$2 c=$3", "one two")
	want := "a=one b=two c="
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSubstitute_MultiDigitPositional(t *testing.T) {
	args := "a b c d e f g h i j k"
	got := Substitute("tenth=$10 eleventh=$11", args)
	want := "tenth=j eleventh=k"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSubstitute_NoSubstitutionsLeftUntouched(t *testing.T) {
	// A template with no $-tokens passes through verbatim. Tests
	// the fast path and pins behavior so a future "always rewrite"
	// optimization doesn't accidentally normalize whitespace.
	got := Substitute("plain text  with   spaces", "ignored")
	want := "plain text  with   spaces"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSubstitute_EmptyArgs(t *testing.T) {
	// Empty args: $ARGUMENTS and $@ both render empty; $N renders
	// empty for any N. The template's surrounding text survives.
	got := Substitute("[$1][$@][$ARGUMENTS]", "")
	want := "[][][]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSubstitute_NonDigitDollarsPassThrough(t *testing.T) {
	// `$foo`, `$bar`, `$_x` — a `$` followed by anything that is not
	// a digit, `@`, or `ARGUMENTS` is a literal. The template can
	// embed env-style references the LLM is meant to interpret.
	// Note: `$5` would still be a positional ref. Templates that
	// need a literal `$5` (e.g. a price) cannot have one — accepted
	// limitation, documented in template.go.
	got := Substitute("env=$HOME path=$XDG_CONFIG_HOME literal=$_", "ignored")
	want := "env=$HOME path=$XDG_CONFIG_HOME literal=$_"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSubstitute_DigitDollarIsAlwaysPositional(t *testing.T) {
	// Documented gotcha: `$N` is unambiguously a positional ref.
	// "$5.99" with fewer than 5 args resolves $5 to empty and
	// leaves ".99" alone. Pinning this so a future "literal $N"
	// escape proposal has to consciously change the test.
	got := Substitute("price is $5.99", "a b")
	want := "price is .99"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSubstitute_ArgumentsBoundary(t *testing.T) {
	// $ARGUMENTSish must NOT match $ARGUMENTS — the \b in the
	// regex blocks the partial. Locks the boundary so a future
	// alternation reorder can't silently break it.
	got := Substitute("$ARGUMENTSish vs $ARGUMENTS done", "hi")
	want := "$ARGUMENTSish vs hi done"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSubstitute_NoRecursiveExpansion(t *testing.T) {
	// If a positional value contains "$ARGUMENTS", that literal
	// passes through unchanged — single-pass substitution only.
	got := Substitute("first=$1 then=$2", "$ARGUMENTS something")
	want := "first=$ARGUMENTS then=something"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
