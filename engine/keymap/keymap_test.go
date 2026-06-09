package keymap

import "testing"

func TestDefaultBindings_AllActionsHaveLabels(t *testing.T) {
	bindings := DefaultBindings()
	if len(bindings) == 0 {
		t.Fatal("DefaultBindings returned no bindings")
	}
	for _, b := range bindings {
		if b.Label == "" {
			t.Errorf("binding for action %d has empty label", b.Action)
		}
		if len(b.Keys) == 0 {
			t.Errorf("binding %q has no keys", b.Label)
		}
		if b.Category == "" {
			t.Errorf("binding %q has empty category", b.Label)
		}
	}
}

func TestDefaultBindings_NoDuplicateActions(t *testing.T) {
	bindings := DefaultBindings()
	seen := make(map[Action]string)
	for _, b := range bindings {
		if prev, ok := seen[b.Action]; ok {
			t.Errorf("duplicate action %d: %q and %q", b.Action, prev, b.Label)
		}
		seen[b.Action] = b.Label
	}
}

func TestCategoryOrder_CoversAllCategories(t *testing.T) {
	order := CategoryOrder()
	bindings := DefaultBindings()

	usedCats := make(map[Category]bool)
	for _, b := range bindings {
		usedCats[b.Category] = true
	}

	orderSet := make(map[Category]bool)
	for _, c := range order {
		orderSet[c] = true
	}

	for cat := range usedCats {
		if !orderSet[cat] {
			t.Errorf("category %q used in bindings but missing from CategoryOrder", cat)
		}
	}
}
