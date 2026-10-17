package kimi

import "testing"

// builtinCatalog is the offline catalogue, which is all an operator sees when the
// CLI is absent. This pins both halves of the modelmeta wiring: an id whose
// installed-client cache publishes a context window gains exactly that number,
// and an id with no client source gains nothing at all rather than a guess.
func TestBuiltinCatalogCarriesOnlySourcedNumbers(t *testing.T) {
	models := builtinCatalog()
	if len(models) != 5 {
		t.Fatalf("builtinCatalog has %d entries, want 5", len(models))
	}
	// The installed kimi-desktop cache publishes maxContextSize for these three
	// ids but publishes no output cap for any of them.
	sourced := map[string]int64{
		"k3-agent":       1000000,
		"k3-agent-ultra": 1000000,
		"k2d6-agent":     262144,
	}
	for _, m := range models {
		ctx, hasCtx := m.Extra["context_length"]
		out, hasOut := m.Extra["max_output_tokens"]
		if want, ok := sourced[m.ID]; ok {
			if !hasCtx || ctx != want {
				t.Errorf("%s context_length = %v, want the client-published %d", m.ID, ctx, want)
			}
		} else {
			if hasCtx {
				t.Errorf("%s grew a context_length of %v; no client source covers this id", m.ID, ctx)
			}
		}
		if hasOut {
			t.Errorf("%s grew a max_output_tokens of %v; the client cache publishes no output cap", m.ID, out)
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
