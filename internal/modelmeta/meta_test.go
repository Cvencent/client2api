package modelmeta

import "testing"

func TestMergeFillsOnlyEmptyFields(t *testing.T) {
	vendor := Meta{ContextLength: 111, Source: SourceVendor}
	fallback := Meta{
		ContextLength:   222,
		MaxOutputTokens: 333,
		Efforts:         []string{"low", "high"},
		DefaultEffort:   "high",
		Source:          SourceStatic,
	}
	got := Merge(vendor, fallback)

	if got.ContextLength != 111 {
		t.Errorf("ContextLength = %d, want the vendor's 111", got.ContextLength)
	}
	if got.MaxOutputTokens != 333 {
		t.Errorf("MaxOutputTokens = %d, want the fallback's 333", got.MaxOutputTokens)
	}
	if len(got.Efforts) != 2 || got.Efforts[0] != "low" || got.Efforts[1] != "high" {
		t.Errorf("Efforts = %v, want [low high]", got.Efforts)
	}
	if got.DefaultEffort != "high" {
		t.Errorf("DefaultEffort = %q, want high", got.DefaultEffort)
	}
	if got.SourceOf(FieldContextLength) != SourceVendor {
		t.Errorf("context source = %q, want %q", got.SourceOf(FieldContextLength), SourceVendor)
	}
	if got.SourceOf(FieldMaxOutputTokens) != SourceStatic {
		t.Errorf("output source = %q, want %q", got.SourceOf(FieldMaxOutputTokens), SourceStatic)
	}
	if got.Source != SourceVendor {
		t.Errorf("Source = %q, want the strongest present source %q", got.Source, SourceVendor)
	}
	if vendor.MaxOutputTokens != 0 || vendor.FieldSources != nil {
		t.Errorf("Merge mutated its vendor argument: %+v", vendor)
	}
}

func TestMergeDoesNotAliasFallbackSlices(t *testing.T) {
	fallback := Meta{Efforts: []string{"low", "high"}, Source: SourceStatic}
	got := Merge(Meta{Source: SourceVendor}, fallback)
	got.Efforts[0] = "mutated"
	if fallback.Efforts[0] != "low" {
		t.Fatalf("Merge aliased the fallback slice: %v", fallback.Efforts)
	}
}

func TestMergeDropsUnsupportedDefaultEffort(t *testing.T) {
	// The vendor says the model only takes [low], the fallback's default is high:
	// a default the model does not accept must not be advertised.
	got := Merge(
		Meta{Efforts: []string{"low"}, Source: SourceVendor},
		Meta{Efforts: []string{"low", "high"}, DefaultEffort: "high", Source: SourceStatic},
	)
	if got.DefaultEffort != "" {
		t.Fatalf("DefaultEffort = %q, want it dropped (not in %v)", got.DefaultEffort, got.Efforts)
	}
	if got.SourceOf(FieldDefaultEffort) != "" {
		t.Fatalf("dropped field kept provenance: %q", got.SourceOf(FieldDefaultEffort))
	}
	if len(got.Efforts) != 1 || got.Efforts[0] != "low" {
		t.Fatalf("Efforts = %v, want the vendor's [low]", got.Efforts)
	}
}

func TestMergeKeepsDefaultWhenNoEffortsAtAll(t *testing.T) {
	// A default without a vocabulary is meaningless; the reference omits the whole
	// field rather than emitting a default nobody can use.
	got := Merge(Meta{}, Meta{DefaultEffort: "high", Source: SourceStatic})
	if got.DefaultEffort != "" {
		t.Fatalf("DefaultEffort = %q, want empty", got.DefaultEffort)
	}
}

func TestMergeIsIdempotent(t *testing.T) {
	a := Meta{ContextLength: 10, Source: SourceVendor}
	b := Meta{ContextLength: 20, MaxOutputTokens: 30, Efforts: []string{"high"}, DefaultEffort: "high", Source: SourceStatic}
	once := Merge(a, b)
	twice := Merge(once, b)
	if !once.Equal(twice) {
		t.Fatalf("Merge is not idempotent:\n once=%+v\ntwice=%+v", once, twice)
	}
}

func TestMergeZeroFallbackIsPassThrough(t *testing.T) {
	vendor := Meta{ContextLength: 7, Source: SourceVendor}
	got := Merge(vendor, Meta{})
	if got.ContextLength != 7 {
		t.Fatalf("Merge(vendor, zero) lost the vendor value: %+v", got)
	}
	if got.Has(FieldMaxOutputTokens) || len(got.Efforts) != 0 {
		t.Fatalf("Merge(vendor, zero) invented fields: %+v", got)
	}
	// Provenance is seeded for the vendor's own field, so a caller can still see
	// where the number came from after the merge.
	if got.SourceOf(FieldContextLength) != SourceVendor {
		t.Fatalf("context provenance = %q, want %q", got.SourceOf(FieldContextLength), SourceVendor)
	}
	if got.Source != SourceVendor {
		t.Fatalf("aggregate Source = %q, want %q", got.Source, SourceVendor)
	}
}

func TestMergeProvenanceFromFieldSources(t *testing.T) {
	// Per-field provenance on the fallback must survive, so a cache-sourced number
	// is not relabelled as a static one.
	fallback := Meta{
		MaxOutputTokens: 4096,
		Source:          SourceCache,
		FieldSources:    map[string]string{FieldMaxOutputTokens: SourceModelsDev},
	}
	got := Merge(Meta{Source: SourceVendor}, fallback)
	if got.SourceOf(FieldMaxOutputTokens) != SourceModelsDev {
		t.Fatalf("output source = %q, want %q", got.SourceOf(FieldMaxOutputTokens), SourceModelsDev)
	}
	if got.Source != SourceModelsDev {
		t.Fatalf("Source = %q, want %q", got.Source, SourceModelsDev)
	}
}

func TestMetaHasAndSourceOf(t *testing.T) {
	m := Meta{ContextLength: 1, Efforts: []string{"high"}, Source: SourceVendor}
	if !m.Has(FieldContextLength) || !m.Has(FieldEfforts) {
		t.Fatalf("Has() missed a set field: %+v", m)
	}
	if m.Has(FieldMaxOutputTokens) || m.Has(FieldDefaultEffort) {
		t.Fatalf("Has() reported an unset field: %+v", m)
	}
	if m.SourceOf(FieldMaxOutputTokens) != "" {
		t.Fatalf("SourceOf on an unset field = %q, want empty", m.SourceOf(FieldMaxOutputTokens))
	}
	if (Meta{}).IsZero() != true || m.IsZero() {
		t.Fatal("IsZero() is wrong")
	}
}

func TestSourceRankOrdersLayers(t *testing.T) {
	ranked := []string{SourceVendor, SourceStatic, SourceModelsDev, SourceCache, SourceFallback}
	for i := 1; i < len(ranked); i++ {
		if sourceRank(ranked[i-1]) >= sourceRank(ranked[i]) {
			t.Fatalf("sourceRank(%q) !< sourceRank(%q)", ranked[i-1], ranked[i])
		}
	}
	if sourceRank("something-else") <= sourceRank(SourceFallback) {
		t.Fatal("an unknown source must rank last, not override a known one")
	}
}

func TestStrongestSourceIsUnknownOnlyWhenAllUnknown(t *testing.T) {
	if got := strongestSource(map[string]string{FieldContextLength: SourceCache, FieldEfforts: SourceStatic}); got != SourceStatic {
		t.Fatalf("strongestSource = %q, want %q", got, SourceStatic)
	}
	if got := strongestSource(nil); got != "" {
		t.Fatalf("strongestSource(nil) = %q, want empty", got)
	}
}

func TestDedupeStrings(t *testing.T) {
	got := dedupeStrings([]string{"low", "", "low", " high ", "high"})
	want := []string{"low", "high"}
	if len(got) != len(want) {
		t.Fatalf("dedupeStrings = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dedupeStrings = %v, want %v", got, want)
		}
	}
	if dedupeStrings([]string{"", "  "}) != nil {
		t.Fatal("dedupeStrings of only empties should be nil")
	}
}

func TestCloneIsDeep(t *testing.T) {
	src := Meta{
		Efforts:      []string{"high"},
		FieldSources: map[string]string{FieldEfforts: SourceStatic},
	}
	cp := src.Clone()
	cp.Efforts[0] = "x"
	cp.FieldSources[FieldEfforts] = "y"
	if src.Efforts[0] != "high" || src.FieldSources[FieldEfforts] != SourceStatic {
		t.Fatalf("Clone aliased its source: %+v", src)
	}
}
