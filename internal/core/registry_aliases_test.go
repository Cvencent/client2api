package core

import "testing"

// SetAliases replaces the table rather than merging into it.  That is the whole
// reason a reload can be trusted after an alias is deleted: an additive reload
// would let the removed name keep resolving until the next start, which is
// exactly the "I deleted it and it still works" surprise the panel is meant to
// remove.
func TestSetAliasesReplacesTheWholeTable(t *testing.T) {
	r := NewRegistry()
	r.SetAliases(map[string]string{"a": "cline/m1", "b": "cline/m2"})
	if got, ok := r.Alias("b"); !ok || got != "cline/m2" {
		t.Fatalf("Alias(b) = %q, %v; want cline/m2, true", got, ok)
	}

	r.SetAliases(map[string]string{"a": "cline/m3"})
	if _, ok := r.Alias("b"); ok {
		t.Error("an alias deleted by the reload still resolves")
	}
	if got, ok := r.Alias("a"); !ok || got != "cline/m3" {
		t.Errorf("Alias(a) = %q, %v; want the replaced target cline/m3, true", got, ok)
	}

	// A blank side names nothing: neither an empty alias nor an empty target is
	// a mapping, and keeping either would make Resolve rewrite a request to "".
	r.SetAliases(map[string]string{"": "cline/m1", "c": ""})
	if got := r.Aliases(); len(got) != 0 {
		t.Errorf("Aliases() = %v, want no entries for blank names or targets", got)
	}
}

// Aliases() hands back a copy: a caller walking the live table must not be able
// to mutate it from under a request.
func TestAliasesReturnsACopy(t *testing.T) {
	r := NewRegistry()
	r.SetAliases(map[string]string{"a": "cline/m1"})
	got := r.Aliases()
	got["a"] = "mutated"
	got["b"] = "cline/m2"
	if target, ok := r.Alias("a"); !ok || target != "cline/m1" {
		t.Errorf("Alias(a) = %q, %v; want the table untouched by the caller", target, ok)
	}
	if _, ok := r.Alias("b"); ok {
		t.Error("Aliases() returned the live map, not a copy")
	}
}
