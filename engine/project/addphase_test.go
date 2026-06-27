package project

import (
	"strings"
	"testing"
)

// TestAddPhase_AutoNumbers verifies a bare descriptive title is
// numbered one past the highest existing phase number and appended as
// a new h1 root that Validate accepts.
func TestAddPhase_AutoNumbers(t *testing.T) {
	tree := Parse("---\nproject: P\n---\n# Phase 1: Foundation\n## F\n- [ ] t\n")

	full, err := tree.AddPhase("Polish")
	if err != nil {
		t.Fatalf("AddPhase: %v", err)
	}
	if full != "Phase 2: Polish" {
		t.Errorf("full = %q, want %q", full, "Phase 2: Polish")
	}
	if len(tree.Roots) != 2 {
		t.Fatalf("Roots = %d, want 2", len(tree.Roots))
	}
	last := tree.Roots[1]
	if !last.IsHeading || last.Depth != 0 || last.Title != "Phase 2: Polish" {
		t.Errorf("appended root = %+v, want h1 depth-0 'Phase 2: Polish'", last)
	}
	if errs := Validate(tree); errs != nil {
		t.Errorf("tree must validate after AddPhase: %v", errs)
	}
}

// TestAddPhase_VerbatimWhenAlreadyNumbered verifies a title already in
// "Phase N: Title" form is used verbatim — the caller's explicit
// numbering wins over auto-numbering.
func TestAddPhase_VerbatimWhenAlreadyNumbered(t *testing.T) {
	tree := Parse("# Phase 1: A\n")

	full, err := tree.AddPhase("Phase 7: Custom")
	if err != nil {
		t.Fatalf("AddPhase: %v", err)
	}
	if full != "Phase 7: Custom" {
		t.Errorf("full = %q, want verbatim %q", full, "Phase 7: Custom")
	}
}

// TestAddPhase_NumbersPastLegacyBarePhases verifies the next number
// falls back to the root count when existing roots carry no parseable
// number (a tree seeded by an older project_init that wrote bare h1
// titles), so a fresh append never collides with an implicit position.
func TestAddPhase_NumbersPastLegacyBarePhases(t *testing.T) {
	tree := Parse("# Foundation\n# Movement\n")

	full, err := tree.AddPhase("Polish")
	if err != nil {
		t.Fatalf("AddPhase: %v", err)
	}
	if full != "Phase 3: Polish" {
		t.Errorf("full = %q, want %q (root count + 1)", full, "Phase 3: Polish")
	}
}

// TestAddPhase_RejectsEmpty verifies a blank title is rejected and the
// tree is left unchanged.
func TestAddPhase_RejectsEmpty(t *testing.T) {
	tree := Parse("# Phase 1: A\n")

	if _, err := tree.AddPhase("   "); err == nil {
		t.Fatal("AddPhase(blank) = nil error, want rejection")
	}
	if len(tree.Roots) != 1 {
		t.Errorf("Roots = %d, want unchanged 1", len(tree.Roots))
	}
}

// TestAddPhase_RejectsDuplicateTitle verifies an exact full-title
// collision (case-insensitive) is rejected so two identical phases
// can't be created.
func TestAddPhase_RejectsDuplicateTitle(t *testing.T) {
	tree := Parse("# Phase 1: Polish\n")

	// A bare "Polish" would auto-number to "Phase 2: Polish" — distinct
	// and allowed. The collision is on an explicit canonical title that
	// case-insensitively matches the existing one ("Phase 1: polish" vs
	// "Phase 1: Polish"); the "Phase" keyword stays capitalised so the
	// title is taken verbatim rather than re-numbered.
	if _, err := tree.AddPhase("Phase 1: polish"); err == nil {
		t.Fatal("AddPhase(duplicate) = nil error, want rejection")
	}
	if len(tree.Roots) != 1 {
		t.Errorf("Roots = %d, want unchanged 1", len(tree.Roots))
	}
}

// TestFormatPhaseHeading verifies the shared formatter: verbatim when
// already canonical, prefixed otherwise, trimmed always.
func TestFormatPhaseHeading(t *testing.T) {
	cases := []struct {
		num   int
		title string
		want  string
	}{
		{3, "Polish", "Phase 3: Polish"},
		{1, "  Foundation  ", "Phase 1: Foundation"},
		{9, "Phase 4: Explicit", "Phase 4: Explicit"},
	}
	for _, c := range cases {
		if got := FormatPhaseHeading(c.num, c.title); got != c.want {
			t.Errorf("FormatPhaseHeading(%d, %q) = %q, want %q", c.num, c.title, got, c.want)
		}
	}
}

// TestAddPhase_SerializesToValidHeading verifies a round-trip through
// Serialize emits the numbered h1 the parser and validator expect.
func TestAddPhase_SerializesToValidHeading(t *testing.T) {
	tree := Parse("---\nproject: P\n---\n# Phase 1: A\n")
	if _, err := tree.AddPhase("B"); err != nil {
		t.Fatalf("AddPhase: %v", err)
	}
	out := Serialize(tree)
	if !strings.Contains(out, "# Phase 2: B") {
		t.Errorf("serialized output missing '# Phase 2: B':\n%s", out)
	}
	if errs := Validate(Parse(out)); errs != nil {
		t.Errorf("re-parsed serialized tree must validate: %v", errs)
	}
}
