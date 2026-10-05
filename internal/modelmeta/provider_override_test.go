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
