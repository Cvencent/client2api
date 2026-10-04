package modelmeta

import (
	"encoding/json"
	"testing"
)

func TestFromExtraReadsLiveValues(t *testing.T) {
	extra := map[string]any{
		"context_length":    200000,
		"max_output_tokens": int64(131072),
		"supported_efforts": []string{"low", "high"},
		"default_effort":    "high",
	}
	m := FromExtra(extra, KeysCanonical, "")
	if m.ContextLength != 200000 || m.MaxOutputTokens != 131072 {
		t.Fatalf("numbers = %d/%d", m.ContextLength, m.MaxOutputTokens)
	}
	if !equalStrings(m.Efforts, []string{"low", "high"}) || m.DefaultEffort != "high" {
		t.Fatalf("efforts = %v default %q", m.Efforts, m.DefaultEffort)
	}
	if m.Source != SourceVendor || m.SourceOf(FieldContextLength) != SourceVendor {
		t.Fatalf("provenance = %q/%q", m.Source, m.SourceOf(FieldContextLength))
	}
}

func TestFromExtraAcceptsJSONDecodedShapes(t *testing.T) {
	// A sidecar list decoded from JSON gives float64 and []any, which must read the
	// same as the Go-native shapes.
	var decoded map[string]any
	raw := `{"context_window": 1050000.0, "max_tokens": 128000, "reasoning_efforts": ["medium"], "default_effort": "medium"}`
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatal(err)
	}
	m := FromExtra(decoded, KeysWorkbuddy, "")
	if m.ContextLength != 1050000 || m.MaxOutputTokens != 128000 {
		t.Fatalf("numbers = %d/%d", m.ContextLength, m.MaxOutputTokens)
	}
	if !equalStrings(m.Efforts, []string{"medium"}) || m.DefaultEffort != "medium" {
		t.Fatalf("efforts = %v default %q", m.Efforts, m.DefaultEffort)
	}
}

func TestFromExtraIgnoresUnusableValues(t *testing.T) {
	cases := []map[string]any{
		{"context_length": 0},
		{"context_length": -1},
		{"context_length": "200000"},
		{"context_length": 1.5},
		{"max_output_tokens": nil},
		{"supported_efforts": []any{}},
		{"supported_efforts": []any{"high", 7}},
		{"default_effort": 5},
	}
	for i, extra := range cases {
		if m := FromExtra(extra, KeysCanonical, ""); !m.IsZero() {
			t.Errorf("case %d (%v) read %+v, want zero", i, extra, m)
		}
	}
	if m := FromExtra(nil, KeysCanonical, ""); !m.IsZero() {
		t.Fatalf("a nil map produced %+v", m)
	}
}

func TestFromExtraDropsUnsupportedDefault(t *testing.T) {
	m := FromExtra(map[string]any{
		"supported_efforts": []string{"high"},
		"default_effort":    "max",
	}, KeysCanonical, "")
	if m.DefaultEffort != "" {
		t.Fatalf("default = %q, want it dropped: max is not in the vocabulary", m.DefaultEffort)
	}
	if !equalStrings(m.Efforts, []string{"high"}) {
		t.Fatalf("efforts = %v", m.Efforts)
	}
}

func TestFromExtraHonoursAnExplicitSource(t *testing.T) {
	m := FromExtra(map[string]any{"context_length": 5}, KeysCanonical, SourceCache)
	if m.SourceOf(FieldContextLength) != SourceCache {
		t.Fatalf("provenance = %q, want cache", m.SourceOf(FieldContextLength))
	}
}

func TestFillExtraOnlyFillsGaps(t *testing.T) {
	extra := map[string]any{
		"context_window": 4242, // live vendor value: must survive untouched
		"max_tokens":     0,    // unusable: counts as missing
	}
	wrote := FillExtra(extra, Meta{
		ContextLength:   1000000,
		MaxOutputTokens: 131072,
		Efforts:         []string{"high", "xhigh"},
		DefaultEffort:   "high",
	}, KeysWorkbuddy)

	if extra["context_window"] != 4242 {
		t.Fatalf("the live value was overwritten: %v", extra["context_window"])
	}
	if extra["max_tokens"] != int64(131072) {
		t.Fatalf("max_tokens = %v, want the fallback 131072", extra["max_tokens"])
	}
	if !equalStrings(extraStrings(extra["reasoning_efforts"]), []string{"high", "xhigh"}) {
		t.Fatalf("efforts = %v", extra["reasoning_efforts"])
	}
	if extra["default_effort"] != "high" {
		t.Fatalf("default = %v", extra["default_effort"])
	}
	if !equalStrings(wrote, []string{FieldMaxOutputTokens, FieldEfforts, FieldDefaultEffort}) {
		t.Fatalf("wrote = %v", wrote)
	}
}

func TestFillExtraNeverWritesAZeroOrEmptyValue(t *testing.T) {
	extra := map[string]any{}
	wrote := FillExtra(extra, Meta{Source: SourceStatic}, KeysCanonical)
	if len(wrote) != 0 || len(extra) != 0 {
		t.Fatalf("a zero Meta wrote %v into %v", wrote, extra)
	}
	// A Meta that only knows an effort list must not fabricate the other fields.
	extra = map[string]any{}
	FillExtra(extra, Meta{Efforts: []string{"high"}, Source: SourceStatic}, KeysCanonical)
	if _, ok := extra["context_length"]; ok {
		t.Fatal("an unknown context window was written")
	}
	if _, ok := extra["max_output_tokens"]; ok {
		t.Fatal("an unknown output limit was written")
	}
}

func TestFillExtraLeavesExistingEffortsAlone(t *testing.T) {
	extra := map[string]any{"supported_efforts": []string{"medium"}}
	FillExtra(extra, Meta{Efforts: []string{"low", "high"}, Source: SourceStatic}, KeysCanonical)
	if !equalStrings(extraStrings(extra["supported_efforts"]), []string{"medium"}) {
		t.Fatalf("efforts = %v, want the vendor's own list", extra["supported_efforts"])
	}
	// An empty or junk existing value does count as missing.
	extra = map[string]any{"supported_efforts": []any{}}
	FillExtra(extra, Meta{Efforts: []string{"low", "high"}, Source: SourceStatic}, KeysCanonical)
	if !equalStrings(extraStrings(extra["supported_efforts"]), []string{"low", "high"}) {
		t.Fatalf("efforts = %v", extra["supported_efforts"])
	}
}

func TestFillExtraDoesNotAliasTheCallerSlice(t *testing.T) {
	m := Meta{Efforts: []string{"high"}}
	extra := map[string]any{}
	FillExtra(extra, m, KeysCanonical)
	got := extra["supported_efforts"].([]string)
	got[0] = "mutated"
	if m.Efforts[0] != "high" {
		t.Fatal("FillExtra aliased the Meta's effort slice")
	}
}

func TestFillExtraNilMapIsInert(t *testing.T) {
	if wrote := FillExtra(nil, Meta{ContextLength: 1}, KeysCanonical); wrote != nil {
		t.Fatalf("wrote = %v", wrote)
	}
}

func TestFillExtraRespectsEmptyKeys(t *testing.T) {
	// A module that does not emit an efforts key must not suddenly get one.
	extra := map[string]any{}
	FillExtra(extra, Meta{ContextLength: 10, Efforts: []string{"high"}}, ExtraKeys{ContextLength: "ctx"})
	if extra["ctx"] != int64(10) {
		t.Fatalf("ctx = %v", extra["ctx"])
	}
	if len(extra) != 1 {
		t.Fatalf("extra = %v, want only the configured key", extra)
	}
}

func TestFromExtraThenFillExtraRoundTrip(t *testing.T) {
	// The pair is the whole reason a module does not have to duplicate the
	// precedence rules: read the live map, merge, write back only the gaps.
	extra := map[string]any{"context_window": 200000}
	vendor := FromExtra(extra, KeysWorkbuddy, "")
	merged := Merge(vendor, Meta{
		ContextLength:   1000000,
		MaxOutputTokens: 131072,
		Efforts:         []string{"high"},
		Source:          SourceStatic,
	})
	FillExtra(extra, merged, KeysWorkbuddy)

	if extra["context_window"] != 200000 {
		t.Fatalf("context = %v, want the vendor 200000", extra["context_window"])
	}
	if extra["max_tokens"] != int64(131072) {
		t.Fatalf("max_tokens = %v", extra["max_tokens"])
	}
	if merged.SourceOf(FieldContextLength) != SourceVendor || merged.SourceOf(FieldMaxOutputTokens) != SourceStatic {
		t.Fatalf("provenance = %v", merged.FieldSources)
	}
}
