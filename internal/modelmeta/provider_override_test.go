package modelmeta

import (
	"path/filepath"
	"testing"
)

func TestProviderOverrideWinsOverVendorAndStatic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model_context.json")
	s, err := OpenOverrideStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("zcode", "glm-5.3-flash", 222222, 33333); err != nil {
		t.Fatal(err)
	}
	p := New(Options{Client: "zcode", Overrides: s})
	got := p.Merge("glm-5.3-flash", Meta{ContextLength: 111111, MaxOutputTokens: 22222, Source: SourceVendor})
	if got.ContextLength != 222222 || got.MaxOutputTokens != 33333 {
		t.Fatalf("got %+v", got)
	}
	if got.SourceOf(FieldContextLength) != SourceManual || got.SourceOf(FieldMaxOutputTokens) != SourceManual {
		t.Fatalf("sources = %+v", got.FieldSources)
	}
}

func TestProviderEffortOverrideWinsOverVendorAndKeepsManualProvenance(t *testing.T) {
	s, err := OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetEfforts("workbuddy", "cn:glm-5.3", []string{"low", "high", "max"}, "max"); err != nil {
		t.Fatal(err)
	}
	p := New(Options{Client: "workbuddy", Overrides: s})
	got := p.Merge("cn:glm-5.3", Meta{
		Efforts:       []string{"medium"},
		DefaultEffort: "medium",
		Source:        SourceVendor,
	})
	if !equalStrings(got.Efforts, []string{"low", "high", "max"}) {
		t.Fatalf("efforts = %v, want the manual [low high max]", got.Efforts)
	}
	if got.DefaultEffort != "max" {
		t.Fatalf("default effort = %q, want the manual max", got.DefaultEffort)
	}
	if got.SourceOf(FieldEfforts) != SourceManual || got.SourceOf(FieldDefaultEffort) != SourceManual {
		t.Fatalf("effort sources = %+v, want manual", got.FieldSources)
	}
}
