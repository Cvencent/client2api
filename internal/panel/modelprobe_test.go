package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/gateway"
	"client2api/internal/modelmeta"
)

// fakeProbes is a ProbeSource backed by a map, so the panel tests need no
// gateway instance.
type fakeProbes struct {
	entries map[string]map[string]gateway.ProbeRecord
}

func newFakeProbes() *fakeProbes {
	return &fakeProbes{entries: map[string]map[string]gateway.ProbeRecord{}}
}

func (f *fakeProbes) Snapshot() map[string]map[string]gateway.ProbeRecord {
	out := map[string]map[string]gateway.ProbeRecord{}
	for c, models := range f.entries {
		m := map[string]gateway.ProbeRecord{}
		for k, v := range models {
			m[k] = v
		}
		out[c] = m
	}
	return out
}

// Clear mirrors the real store: it leaves a tombstone instead of deleting, so
// a model whose context length is known does not immediately look settled again.
func (f *fakeProbes) Clear(client, model string) bool {
	models := f.entries[client]
	if models == nil {
		models = map[string]gateway.ProbeRecord{}
		f.entries[client] = models
	}
	if prev, ok := models[model]; ok && prev.Outcome == "cleared" {
		return false
	}
	models[model] = gateway.ProbeRecord{Probed: false, Outcome: "cleared"}
	return true
}

func TestModelProbeClearRemovesOnlyThatModel(t *testing.T) {
	probes := newFakeProbes()
	probes.entries["raccoon"] = map[string]gateway.ProbeRecord{
		"model-a": {Probed: true, Outcome: "accepted", EstimateTokens: 1000},
		"model-b": {Probed: true, Outcome: "refused"},
	}
	h := New(Options{Probes: probes})
	body := `{"client":"raccoon","model":"model-a"}`
	req := httptest.NewRequest(http.MethodPost, "/panel/api/model_probe/clear", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if rec := probes.entries["raccoon"]["model-a"]; rec.Probed {
		t.Fatal("model-a probe must be cleared")
	}
	if _, ok := probes.entries["raccoon"]["model-b"]; !ok {
		t.Fatal("model-b probe must survive")
	}
}

func TestModelProbeClearRejectsBadRequests(t *testing.T) {
	h := New(Options{Probes: newFakeProbes()})
	req := httptest.NewRequest(http.MethodGet, "/panel/api/model_probe/clear", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/panel/api/model_probe/clear", strings.NewReader(`{"model":"m"}`))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("missing client status = %d, want 400", rec2.Code)
	}
}

func TestModelProbeClearLeavesLearnedWindowAndOverride(t *testing.T) {
	probes := newFakeProbes()
	probes.entries["raccoon"] = map[string]gateway.ProbeRecord{
		"model-a": {Probed: true, Outcome: "accepted", EstimateTokens: 1000},
	}
	overrides, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := overrides.Set("raccoon", "model-a", 123456, 0); err != nil {
		t.Fatal(err)
	}
	h := New(Options{Probes: probes, ModelOverrides: overrides})
	req := httptest.NewRequest(http.MethodPost, "/panel/api/model_probe/clear", strings.NewReader(`{"client":"raccoon","model":"model-a"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if got, ok := overrides.Get("raccoon", "model-a"); !ok || got.ContextLength != 123456 {
		t.Fatalf("manual override = %+v ok=%v, want it preserved", got, ok)
	}
}

// A model whose context length is already known counts as probed in the models
// table, even when no vendor round trip was ever made.
func TestProbeViewKnownLengthCountsAsProbed(t *testing.T) {
	probes := newFakeProbes()
	if got := probeViewFor(probes, "raccoon", "model-a", 1_000_000); got == nil || !got.Probed || got.Outcome != probeOutcomeKnown {
		t.Fatalf("known-length view = %+v, want probed/known", got)
	}
	if got := probeViewFor(probes, "raccoon", "model-a", 0); got != nil {
		t.Fatalf("unknown-length view = %+v, want nil", got)
	}
}

// After an explicit clear the row must look unprobed again, even though the
// model still has a known context length.
func TestProbeViewClearedOverridesKnownLength(t *testing.T) {
	probes := newFakeProbes()
	probes.entries["raccoon"] = map[string]gateway.ProbeRecord{
		"model-a": {Probed: false, Outcome: "cleared"},
	}
	if got := probeViewFor(probes, "raccoon", "model-a", 1_000_000); got != nil {
		t.Fatalf("cleared view = %+v, want nil", got)
	}
}
