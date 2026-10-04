package kimi

import "testing"

// builtinCatalog is the offline catalogue, which is all an operator sees when the
// CLI is absent. This pins the honest half of the modelmeta wiring: none of the
// five ids is covered by an authoritative source for a context window or an output
// limit, so none of them gains a number, and an id that was unverified stays
// unverified. A future sourced number would flow through here automatically.
func TestBuiltinCatalogCarriesNoUnsourcedNumbers(t *testing.T) {
	models := builtinCatalog()
	if len(models) != 5 {
		t.Fatalf("builtinCatalog has %d entries, want 5", len(models))
	}
	for _, m := range models {
		if v, ok := m.Extra["context_length"]; ok {
			t.Errorf("%s grew a context_length of %v; no authoritative source covers this id", m.ID, v)
		}
		if v, ok := m.Extra["max_output_tokens"]; ok {
			t.Errorf("%s grew a max_output_tokens of %v; no authoritative source covers this id", m.ID, v)
		}
		if v, ok := m.Extra["supported_efforts"]; ok {
			t.Errorf("%s grew a supported_efforts of %v; no authoritative source covers this id", m.ID, v)
		}
	}
}

// The provenance markers this module already published must survive the wiring
// exactly: an id that was self-marked unverified must not become verified.
func TestBuiltinCatalogKeepsItsProvenanceMarkers(t *testing.T) {
	byID := map[string]map[string]any{}
	for _, m := range builtinCatalog() {
		byID[m.ID] = m.Extra
	}
	verified := map[string]bool{
		"kimi":           true,
		"kimi-k2":        true,
		"k3-agent":       false,
		"k3-agent-ultra": false,
		"k2d6-agent":     false,
	}
	for id, wantVerified := range verified {
		extra, ok := byID[id]
		if !ok {
			t.Fatalf("%s is missing from the builtin catalogue", id)
		}
		if _, marked := extra["verified"]; marked == wantVerified {
			t.Errorf("%s: verified marker is %v, want the marker present only for unverified ids", id, marked)
		}
	}
	if byID["kimi"]["source"] != "builtin" || byID["kimi"]["default"] != true {
		t.Errorf("the default id lost its markers: %v", byID["kimi"])
	}
	for _, id := range []string{"k3-agent", "k3-agent-ultra", "k2d6-agent"} {
		if byID[id]["source"] != "kimi-desktop-model-cache" {
			t.Errorf("%s source = %v", id, byID[id]["source"])
		}
	}
}

// The catalog path must not touch the disk or the network: the provider behind it
// is built with no cache directory and no models.dev layer.
func TestBuiltinCatalogMetadataIsOffline(t *testing.T) {
	if metaProvider.Store() != nil {
		t.Fatal("metaProvider has a cache store; listing models must not touch the disk")
	}
	if metaProvider.RemoteEnabled() {
		t.Fatal("metaProvider has models.dev enabled; listing models must stay offline")
	}
}
