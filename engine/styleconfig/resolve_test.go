package styleconfig

import (
	"os"
	"path/filepath"
	"testing"
)

type resolveTestCase struct {
	name         string
	globalJSON   string
	projectJSON  string
	env          map[string]string
	wantNil      bool   // expect nil Resolved
	wantName     string // expected Resolved.Name
	wantRulesCnt int    // expected number of rules
}

var resolveTests = []resolveTestCase{
	{
		name:         "default active style is clean-code",
		wantName:     "Clean Code",
		wantRulesCnt: 12,
	},
	{
		name:         "env var selects builtin style",
		env:          map[string]string{"JUNTO_STYLE": "idiomatic-go"},
		wantName:     "Idiomatic Go",
		wantRulesCnt: 12,
	},
	{
		name:         "global config selects builtin",
		globalJSON:   `{"active": "solid-hexagonal"}`,
		wantName:     "SOLID + Hexagonal Architecture",
		wantRulesCnt: 11,
	},
	{
		name:         "project overrides global active",
		globalJSON:   `{"active": "solid-hexagonal"}`,
		projectJSON:  `{"active": "ddd"}`,
		wantName:     "Domain-Driven Design",
		wantRulesCnt: 10,
	},
	{
		name:         "env var overrides project active",
		projectJSON:  `{"active": "solid-hexagonal"}`,
		env:          map[string]string{"JUNTO_STYLE": "bdd"},
		wantName:     "Behavior-Driven Development",
		wantRulesCnt: 9,
	},
	{
		name:    "unknown style name returns nil",
		env:     map[string]string{"JUNTO_STYLE": "nonexistent"},
		wantNil: true,
	},
	{
		name: "project adds custom style",
		projectJSON: `{
			"active": "custom",
			"styles": {
				"custom": {
					"name": "Custom Style",
					"rules": [
						{"name": "R1", "instruction": "Do the thing", "enforcement": "hard"}
					]
				}
			}
		}`,
		wantName:     "Custom Style",
		wantRulesCnt: 1,
	},
	{
		name:       "project overrides builtin rules",
		globalJSON: `{"active": "idiomatic-go"}`,
		projectJSON: `{
			"styles": {
				"idiomatic-go": {
					"rules": [
						{"name": "Only Rule", "instruction": "Just this one", "enforcement": "hard"}
					]
				}
			}
		}`,
		wantName:     "Idiomatic Go",
		wantRulesCnt: 1,
	},
	{
		name: "lint_cmd from project config",
		projectJSON: `{
			"active": "custom",
			"styles": {
				"custom": {
					"name": "Lint Test",
					"rules": [{"name": "R1", "instruction": "lint it", "enforcement": "soft"}],
					"lint_cmd": ["golangci-lint run {file}"]
				}
			}
		}`,
		wantName:     "Lint Test",
		wantRulesCnt: 1,
	},
}

// setupResolveTest writes config files and sets env vars for a test case.
// Returns (globalPath, projectRoot).
func setupResolveTest(t *testing.T, tc resolveTestCase) (string, string) {
	t.Helper()
	globalDir := t.TempDir()
	globalPath := filepath.Join(globalDir, "style.json")
	projectRoot := t.TempDir()

	if tc.globalJSON != "" {
		if err := os.WriteFile(globalPath, []byte(tc.globalJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if tc.projectJSON != "" {
		dir := filepath.Join(projectRoot, ".project")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "style.json"), []byte(tc.projectJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range tc.env {
		t.Setenv(k, v)
	}
	return globalPath, projectRoot
}

func TestResolve(t *testing.T) {
	for _, tc := range resolveTests {
		t.Run(tc.name, func(t *testing.T) {
			globalPath, projectRoot := setupResolveTest(t, tc)
			_, resolved := resolveWithPaths(globalPath, projectRoot)
			assertResolved(t, resolved, tc)
		})
	}
}

// assertResolved checks the Resolved value against test case expectations.
func assertResolved(t *testing.T, resolved *Resolved, tc resolveTestCase) {
	t.Helper()
	if tc.wantNil {
		if resolved != nil {
			t.Errorf("expected nil Resolved, got %+v", resolved)
		}
		return
	}
	if resolved == nil {
		t.Fatal("expected non-nil Resolved, got nil")
	}
	if resolved.Name != tc.wantName {
		t.Errorf("Name = %q, want %q", resolved.Name, tc.wantName)
	}
	if len(resolved.Rules) != tc.wantRulesCnt {
		t.Errorf("len(Rules) = %d, want %d", len(resolved.Rules), tc.wantRulesCnt)
	}
}

func TestResolve_malformedJSON(t *testing.T) {
	globalDir := t.TempDir()
	globalPath := filepath.Join(globalDir, "style.json")
	if err := os.WriteFile(globalPath, []byte(`{invalid json`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Malformed JSON is skipped, so the default builtin style is used.
	_, resolved := resolveWithPaths(globalPath, "")
	if resolved == nil {
		t.Fatal("expected default style from builtins, got nil")
	}
	if resolved.Name != "Clean Code" {
		t.Errorf("Name = %q, want default Clean Code", resolved.Name)
	}
}

func TestResolve_missingFile(t *testing.T) {
	// Missing files are skipped, so the default builtin style is used.
	_, resolved := resolveWithPaths("/nonexistent/style.json", "/nonexistent/project")
	if resolved == nil {
		t.Fatal("expected default style from builtins, got nil")
	}
	if resolved.Name != "Clean Code" {
		t.Errorf("Name = %q, want default Clean Code", resolved.Name)
	}
}

func TestLoadBuiltins(t *testing.T) {
	cfg := loadBuiltins()

	if cfg.Active != "clean-code" {
		t.Fatalf("Active = %q, want %q", cfg.Active, "clean-code")
	}

	expected := []string{"bdd", "clean-architecture", "clean-code", "ddd", "idiomatic-go", "solid-hexagonal"}
	names := cfg.StyleNames()
	if len(names) != len(expected) {
		t.Fatalf("StyleNames() = %v, want %v", names, expected)
	}
	for i, name := range names {
		if name != expected[i] {
			t.Errorf("StyleNames()[%d] = %q, want %q", i, name, expected[i])
		}
	}

	// Verify each built-in has a non-empty name and at least one rule.
	for key, style := range cfg.Styles {
		if style.Name == "" {
			t.Errorf("style %q has empty Name", key)
		}
		if len(style.Rules) == 0 {
			t.Errorf("style %q has no rules", key)
		}
		for i, r := range style.Rules {
			if r.Name == "" {
				t.Errorf("style %q rule[%d] has empty Name", key, i)
			}
			if r.Instruction == "" {
				t.Errorf("style %q rule[%d] has empty Instruction", key, i)
			}
			if r.Enforcement != "hard" && r.Enforcement != "soft" {
				t.Errorf("style %q rule[%d] Enforcement = %q, want \"hard\" or \"soft\"", key, i, r.Enforcement)
			}
		}
	}
}

func TestConfigStyleNames(t *testing.T) {
	cfg := &Config{Styles: map[string]Style{
		"b": {Name: "B"}, "a": {Name: "A"}, "c": {Name: "C"},
	}}
	names := cfg.StyleNames()
	want := []string{"a", "b", "c"}
	if len(names) != len(want) {
		t.Fatalf("StyleNames() = %v, want %v", names, want)
	}
	for i, n := range names {
		if n != want[i] {
			t.Errorf("StyleNames()[%d] = %q, want %q", i, n, want[i])
		}
	}
}

func TestConfigStyleNames_empty(t *testing.T) {
	cfg := &Config{}
	if names := cfg.StyleNames(); names != nil {
		t.Errorf("StyleNames() on empty config = %v, want nil", names)
	}
}

func TestMergeConfigs(t *testing.T) {
	dst := &Config{
		Active: "old",
		Styles: map[string]Style{
			"a": {Name: "A", Description: "original", Rules: []Rule{{Name: "R1"}}},
		},
	}
	src := &Config{
		Active: "new",
		Styles: map[string]Style{
			"a": {Description: "updated"},
			"b": {Name: "B", Rules: []Rule{{Name: "R2"}}},
		},
	}
	mergeConfigs(dst, src)

	if dst.Active != "new" {
		t.Errorf("Active = %q, want %q", dst.Active, "new")
	}
	a := dst.Styles["a"]
	if a.Name != "A" {
		t.Errorf("style 'a' Name = %q, want %q (preserved from dst)", a.Name, "A")
	}
	if a.Description != "updated" {
		t.Errorf("style 'a' Description = %q, want %q", a.Description, "updated")
	}
	if len(a.Rules) != 1 || a.Rules[0].Name != "R1" {
		t.Errorf("style 'a' Rules should be preserved (src had empty rules)")
	}
	b, ok := dst.Styles["b"]
	if !ok {
		t.Fatal("style 'b' not added by merge")
	}
	if b.Name != "B" {
		t.Errorf("style 'b' Name = %q, want %q", b.Name, "B")
	}
}

// TestMergeConfigsArchitectureFieldwise verifies the per-field merge
// rules — empty fields preserve dst, non-empty values overwrite, and
// each field merges independently so a project tightening only
// MaxFileLines does not have to repeat MaxFunctionLines etc.
func TestMergeConfigsArchitectureFieldwise(t *testing.T) {
	dst := &Config{Styles: map[string]Style{
		"a": {
			Name: "A",
			Architecture: Architecture{
				MaxFileLines:        300,
				MaxFunctionLines:    20,
				MaxFunctionsPerFile: 15,
				Action:              "block",
			},
		},
	}}
	src := &Config{Styles: map[string]Style{
		"a": {
			Architecture: Architecture{
				MaxFileLines: 500, // override only this field
			},
		},
	}}

	mergeConfigs(dst, src)

	a := dst.Styles["a"].Architecture
	if a.MaxFileLines != 500 {
		t.Errorf("MaxFileLines = %d, want 500 (overwritten)", a.MaxFileLines)
	}
	if a.MaxFunctionLines != 20 {
		t.Errorf("MaxFunctionLines = %d, want 20 (preserved)", a.MaxFunctionLines)
	}
	if a.MaxFunctionsPerFile != 15 {
		t.Errorf("MaxFunctionsPerFile = %d, want 15 (preserved)", a.MaxFunctionsPerFile)
	}
	if a.Action != "block" {
		t.Errorf("Action = %q, want block (preserved)", a.Action)
	}
}

// TestMergeConfigsRejectsInvalidArchitectureAction verifies a typo in
// project/global Architecture.Action does NOT overwrite a valid
// builtin. Without this guard, "blcok" would land in dst, then
// normalisedAction would map it to "warn" — silently demoting a
// block-policy preset to a warn-policy one and changing UX
// behaviour (silent retry vs surface-to-developer).
func TestMergeConfigsRejectsInvalidArchitectureAction(t *testing.T) {
	dst := &Config{Styles: map[string]Style{
		"clean": {
			Name:         "Clean",
			Architecture: Architecture{MaxFileLines: 300, Action: "block"},
		},
	}}
	src := &Config{Styles: map[string]Style{
		"clean": {
			Architecture: Architecture{Action: "blcok"}, // typo
		},
	}}

	mergeConfigs(dst, src)

	if got := dst.Styles["clean"].Architecture.Action; got != "block" {
		t.Errorf("Action = %q, want %q (invalid override must be refused)", got, "block")
	}
}

// TestIsValidArchitectureAction locks in the accepted set so a
// well-meaning "let me also accept 'enforce'" PR fails loud here
// instead of silently expanding the policy surface. Validation is
// case-insensitive and whitespace-tolerant — matching
// [Architecture.normalisedAction]'s contract — so a project config
// with `"Action": "BLOCK"` is accepted by the merge path rather
// than silently dropped.
func TestIsValidArchitectureAction(t *testing.T) {
	cases := []struct {
		action string
		want   bool
	}{
		{"", true},
		{"warn", true},
		{"block", true},
		{"off", true},
		{"WARN", true},    // case-insensitive
		{"Block", true},   // mixed case
		{"  off  ", true}, // whitespace-tolerant
		{"enforce", false},
		{"blcok", false},
		{"warning", false}, // close to a valid value but not equal
	}
	for _, tc := range cases {
		if got := isValidArchitectureAction(tc.action); got != tc.want {
			t.Errorf("isValidArchitectureAction(%q) = %v, want %v", tc.action, got, tc.want)
		}
	}
}

// TestMergeConfigsRejectsWhitespaceOnlyArchitectureAction verifies a
// whitespace-only Action value (typo, accidental space) does NOT
// clear an inherited builtin policy. isValidArchitectureAction
// trims internally and accepts "" as a valid value, so the merge
// gate must canonicalise BEFORE checking — otherwise a "   " in
// project config silently demotes a builtin "block" to "" → "warn".
func TestMergeConfigsRejectsWhitespaceOnlyArchitectureAction(t *testing.T) {
	dst := &Config{Styles: map[string]Style{
		"clean": {
			Name:         "Clean",
			Architecture: Architecture{MaxFileLines: 300, Action: "block"},
		},
	}}
	src := &Config{Styles: map[string]Style{
		"clean": {
			Architecture: Architecture{Action: "   "},
		},
	}}

	mergeConfigs(dst, src)

	if got := dst.Styles["clean"].Architecture.Action; got != "block" {
		t.Errorf("Action = %q, want %q (whitespace-only must not clear inherited policy)",
			got, "block")
	}
}

// TestMergeConfigsCanonicalisesArchitectureAction verifies that a
// case/whitespace variant of a known action is accepted AND stored
// in canonical lowercase form, so downstream string comparisons
// don't have to re-normalise. Without this, a project config
// `"Action": "BLOCK"` would silently fall back to the builtin's
// value because the validity check rejected it as unknown.
func TestMergeConfigsCanonicalisesArchitectureAction(t *testing.T) {
	dst := &Config{Styles: map[string]Style{
		"clean": {
			Name:         "Clean",
			Architecture: Architecture{Action: "warn"},
		},
	}}
	src := &Config{Styles: map[string]Style{
		"clean": {
			Architecture: Architecture{Action: "BLOCK"},
		},
	}}

	mergeConfigs(dst, src)

	if got := dst.Styles["clean"].Architecture.Action; got != "block" {
		t.Errorf("Action = %q, want %q (canonical lowercase from BLOCK)", got, "block")
	}
}

func TestResolveLintCmd(t *testing.T) {
	projectRoot := t.TempDir()
	dir := filepath.Join(projectRoot, ".project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{
		"active": "lint-style",
		"styles": {
			"lint-style": {
				"name": "Lint Style",
				"rules": [{"name": "R1", "instruction": "lint", "enforcement": "hard"}],
				"lint_cmd": ["golangci-lint run {file}", "gofmt -l {file}"]
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, "style.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	_, resolved := resolveWithPaths("", projectRoot)
	if resolved == nil {
		t.Fatal("expected non-nil Resolved")
	}
	if len(resolved.LintCmd) != 2 {
		t.Errorf("len(LintCmd) = %d, want 2", len(resolved.LintCmd))
	}
}
