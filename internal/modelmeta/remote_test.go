package modelmeta

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// testModelsDevDoc deliberately publishes gpt-5.5 twice: once from the preferred
// vendor openai and once from acme, so the preference rule is exercised.
const testModelsDevDoc = `{
  "acme": {
    "models": {
      "gpt-5.5": {"limit": {"context": 1, "output": 1}},
      "glm-5.2": {"limit": {"context": 5, "output": 5}},
      "acme-only": {"limit": {"context": 4096, "output": 512}}
    }
  },
  "openai": {
    "models": {
      "gpt-5.5": {"limit": {"context": 1050000, "output": 128000}}
    }
  },
  "deepseek": {
    "models": {
      "deepseek-v4-flash": {"limit": {"context": 1000000, "output": 384000}}
    }
  }
}`

type countingServer struct {
	*httptest.Server
	hits atomic.Int64
}

func newCountingServer(t *testing.T, status int, delay time.Duration, body string) *countingServer {
	t.Helper()
	cs := &countingServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.hits.Add(1)
		if delay > 0 {
			time.Sleep(delay)
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(cs.Close)
	return cs
}

func TestNewRemoteDefaultsAndOptIn(t *testing.T) {
	if rc := newRemote(nil); rc != nil {
		t.Fatal("a nil RemoteConfig must be disabled")
	}
	if rc := newRemote(&RemoteConfig{}); rc != nil {
		t.Fatal("the zero RemoteConfig must be disabled")
	}
	rc := newRemote(&RemoteConfig{Enabled: true})
	if rc == nil {
		t.Fatal("an opted-in config must build a cache")
	}
	if rc.url != ModelsDevURL {
		t.Errorf("url = %q, want %q", rc.url, ModelsDevURL)
	}
	if rc.timeout != DefaultRemoteTimeout || rc.cooldown != DefaultRemoteCooldown || rc.negTTL != DefaultRemoteNegativeTTL {
		t.Errorf("defaults = %v/%v/%v", rc.timeout, rc.cooldown, rc.negTTL)
	}
	if rc.client == nil {
		t.Fatal("no HTTP client was built")
	}
	if rc.client == http.DefaultClient {
		t.Fatal("http.DefaultClient must never be used: this package must control its own transport")
	}
	// A nil *remoteCache is the disabled state and must be inert, not panic.
	var nilRemote *remoteCache
	if nilRemote.enabled() {
		t.Error("nil remote reported enabled")
	}
	if _, ok := nilRemote.lookup("x"); ok {
		t.Error("nil remote returned a value")
	}
	if nilRemote.alreadyNegated("x") {
		t.Error("nil remote reported a negative")
	}
	nilRemote.noteMiss("x")
	nilRemote.refreshAsync()
	if nilRemote.wait(time.Millisecond) {
		t.Error("nil remote reported a document")
	}
}

func TestRemoteDisabledNeverTouchesTheNetwork(t *testing.T) {
	srv := newCountingServer(t, http.StatusOK, 0, testModelsDevDoc)
	p := New(Options{Client: "kimi", Remote: &RemoteConfig{Enabled: false, URL: srv.URL}, Logf: t.Logf})
	if p.RemoteEnabled() {
		t.Fatal("RemoteEnabled() = true for a disabled config")
	}
	for i := 0; i < 3; i++ {
		if m, ok := p.Lookup("gpt-5.5"); ok {
			t.Fatalf("a disabled remote resolved %+v", m)
		}
	}
	if p.WaitRemote(50 * time.Millisecond) {
		t.Fatal("a disabled remote reported a document")
	}
	if n := srv.hits.Load(); n != 0 {
		t.Fatalf("the disabled remote layer made %d requests; it must make none", n)
	}
}

func TestRemoteUnconfiguredProviderIsOffline(t *testing.T) {
	p := New(Options{Client: "workbuddy"})
	if p.RemoteEnabled() {
		t.Fatal("a provider with no Remote config must be offline")
	}
	if p.WaitRemote(time.Millisecond) {
		t.Fatal("WaitRemote on an offline provider must report false")
	}
	if m, ok := p.Lookup("glm-5.2"); !ok || m.ContextLength != 1000000 {
		t.Fatalf("the static table must work with no remote at all: %+v ok=%v", m, ok)
	}
}

func TestRemoteRefreshesAsynchronously(t *testing.T) {
	srv := newCountingServer(t, http.StatusOK, 0, testModelsDevDoc)
	p := New(Options{
		Client: "kimi",
		Remote: &RemoteConfig{Enabled: true, URL: srv.URL, Cooldown: time.Hour},
	})
	if !p.RemoteEnabled() {
		t.Fatal("RemoteEnabled() = false for an enabled config")
	}
	// The first lookup cannot know the answer: the fetch has not happened yet.
	if m, ok := p.Lookup("gpt-5.5"); ok {
		t.Fatalf("the first lookup resolved %+v without waiting for the network", m)
	}
	if !p.WaitRemote(5 * time.Second) {
		t.Fatal("the async refresh never landed")
	}
	m, ok := p.Lookup("gpt-5.5")
	if !ok {
		t.Fatal("the refreshed document did not serve the model")
	}
	if m.ContextLength != 1050000 || m.MaxOutputTokens != 128000 {
		t.Fatalf("got %d/%d, want the openai copy 1050000/128000", m.ContextLength, m.MaxOutputTokens)
	}
	if m.Source != SourceModelsDev || m.SourceOf(FieldContextLength) != SourceModelsDev {
		t.Fatalf("provenance = %q/%q, want %q", m.Source, m.SourceOf(FieldContextLength), SourceModelsDev)
	}
	if n := srv.hits.Load(); n != 1 {
		t.Fatalf("hits = %d, want exactly 1", n)
	}
}

func TestRemoteNegativeCooldownAfterFailure(t *testing.T) {
	srv := newCountingServer(t, http.StatusInternalServerError, 0, "")
	p := New(Options{
		Client: "kimi",
		Remote: &RemoteConfig{Enabled: true, URL: srv.URL, Cooldown: time.Hour},
	})
	if _, ok := p.Lookup("aaa"); ok {
		t.Fatal("a failing upstream cannot resolve anything")
	}
	if p.WaitRemote(5 * time.Second) {
		t.Fatal("a failed fetch must not report a document")
	}
	// A different model, so the per-model negative cache cannot be what suppresses
	// this: the cooldown is, and it counts the failure.
	if _, ok := p.Lookup("bbb"); ok {
		t.Fatal("a failing upstream cannot resolve anything")
	}
	if n := srv.hits.Load(); n != 1 {
		t.Fatalf("hits = %d, want 1: the negative cooldown must prevent a retry storm", n)
	}
}

func TestRemoteCooldownAppliesAfterSuccess(t *testing.T) {
	srv := newCountingServer(t, http.StatusOK, 0, testModelsDevDoc)
	p := New(Options{
		Client: "kimi",
		Remote: &RemoteConfig{Enabled: true, URL: srv.URL, Cooldown: time.Hour},
	})
	if _, ok := p.Lookup("zzz"); ok {
		t.Fatal("unknown model resolved before the refresh")
	}
	if !p.WaitRemote(5 * time.Second) {
		t.Fatal("refresh did not land")
	}
	// "yyy" is inside no negative entry (it was never asked for) and is not in the
	// document, so only the cooldown can stop a second fetch.
	if _, ok := p.Lookup("yyy"); ok {
		t.Fatal("yyy resolved without a document entry")
	}
	if n := srv.hits.Load(); n != 1 {
		t.Fatalf("hits = %d, want 1 (the cooldown blocks the second attempt)", n)
	}
}

func TestRemoteNegativeTTLIsMeasuredFromTheFirstMiss(t *testing.T) {
	rc := newRemote(&RemoteConfig{Enabled: true, NegativeTTL: time.Hour})
	rc.noteMiss("m")
	rc.mu.Lock()
	first := rc.negatives["m"]
	rc.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	rc.noteMiss("m")
	rc.mu.Lock()
	second := rc.negatives["m"]
	rc.mu.Unlock()
	if !first.Equal(second) {
		t.Fatalf("the miss timestamp was refreshed: %v -> %v", first, second)
	}
	if !rc.alreadyNegated("m") {
		t.Fatal("a fresh miss must be reported as negated")
	}
	if rc.alreadyNegated("other") {
		t.Fatal("an unknown model must not be reported as negated")
	}
}

func TestRemoteNegativeCacheEviction(t *testing.T) {
	rc := newRemote(&RemoteConfig{Enabled: true, NegativeTTL: time.Nanosecond})
	for i := 0; i < remoteNegHardCap+10; i++ {
		rc.noteMiss("m" + string(rune('a'+i%26)) + string(rune('a'+i/26)))
		time.Sleep(time.Microsecond)
	}
	rc.mu.Lock()
	n := len(rc.negatives)
	rc.mu.Unlock()
	if n > remoteNegHardCap {
		t.Fatalf("negative cache grew to %d, want <= %d", n, remoteNegHardCap)
	}
}

func TestRemoteLookupNeverBlocksOnASlowFetch(t *testing.T) {
	srv := newCountingServer(t, http.StatusOK, 1500*time.Millisecond, testModelsDevDoc)
	p := New(Options{
		Client: "kimi",
		Remote: &RemoteConfig{Enabled: true, URL: srv.URL, Cooldown: time.Hour},
	})
	start := time.Now()
	p.Lookup("gpt-5.5")
	if elapsed := time.Since(start); elapsed > 750*time.Millisecond {
		t.Fatalf("Lookup blocked for %v on an in-flight fetch", elapsed)
	}
	if !p.WaitRemote(10 * time.Second) {
		t.Fatal("the slow refresh never landed")
	}
}

func TestIndexModelsDevDocPrefersTrustedVendor(t *testing.T) {
	var doc modelsDevDoc
	if err := json.Unmarshal([]byte(testModelsDevDoc), &doc); err != nil {
		t.Fatal(err)
	}
	idx := indexModelsDevDoc(doc)
	got, ok := idx["gpt-5.5"]
	if !ok {
		t.Fatal("gpt-5.5 was not indexed")
	}
	if got.Provider != "openai" {
		t.Fatalf("provider = %q, want openai over acme", got.Provider)
	}
	if got.Context != 1050000 || got.Output != 128000 {
		t.Fatalf("values = %d/%d, want the openai copy", got.Context, got.Output)
	}
	if e := idx["glm-5.2"]; e.Provider != "acme" || e.Context != 5 {
		t.Fatalf("glm-5.2 = %+v, want the only publisher acme", e)
	}
}

func TestIndexModelsDevDocTieBreaksDeterministically(t *testing.T) {
	doc := modelsDevDoc{
		"zzz": {Models: map[string]modelsDevModel{"m": {Limit: &modelsDevLimit{Context: 2}}}},
		"aaa": {Models: map[string]modelsDevModel{"m": {Limit: &modelsDevLimit{Context: 1}}}},
	}
	for i := 0; i < 20; i++ {
		idx := indexModelsDevDoc(doc)
		if got := idx["m"].Provider; got != "aaa" {
			t.Fatalf("iteration %d picked %q, want the lexicographically smallest provider", i, got)
		}
	}
}

func TestIndexModelsDevDocRejectsDirtyValues(t *testing.T) {
	var doc modelsDevDoc
	raw := `{
	  "acme": {"models": {
	    "huge":    {"limit": {"context": 5000000000}},
	    "negative":{"limit": {"context": -1, "output": -1}},
	    "nooutput":{"limit": {"context": 1000}},
	    "nolimit": {},
	    "openai/gpt-5.5": {"limit": {"context": 42, "output": 7}}
	  }}
	}`
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	idx := indexModelsDevDoc(doc)
	if _, ok := idx["huge"]; ok {
		t.Error("an implausible context value must be rejected")
	}
	if _, ok := idx["negative"]; ok {
		t.Error("negative limits must be rejected")
	}
	if _, ok := idx["nolimit"]; ok {
		t.Error("a model with no limit block must not be indexed")
	}
	e, ok := idx["nooutput"]
	if !ok || e.Context != 1000 || e.Output != 0 {
		t.Fatalf("nooutput = %+v ok=%v, want context 1000 and no output", e, ok)
	}
	if e, ok := idx["gpt-5.5"]; !ok || e.Context != 42 {
		t.Fatalf("a vendor-prefixed id must be indexed under its bare id: %+v ok=%v", e, ok)
	}
}

func TestRemoteLookupOfProviderPrefixedRequest(t *testing.T) {
	var doc modelsDevDoc
	if err := json.Unmarshal([]byte(testModelsDevDoc), &doc); err != nil {
		t.Fatal(err)
	}
	rc := newRemote(&RemoteConfig{Enabled: true})
	rc.index = indexModelsDevDoc(doc)
	rc.loaded = true
	m, ok := rc.lookup("openai/gpt-5.5")
	if !ok {
		t.Fatal("a vendor-prefixed request should resolve through the bare id")
	}
	if m.ContextLength != 1050000 {
		t.Fatalf("context = %d, want 1050000", m.ContextLength)
	}
	if m.SourceOf(FieldContextLength) != SourceModelsDev {
		t.Fatalf("provenance = %q", m.SourceOf(FieldContextLength))
	}
}

func TestRemoteFetchRejectsNonOKStatus(t *testing.T) {
	srv := newCountingServer(t, http.StatusTeapot, 0, "")
	rc := newRemote(&RemoteConfig{Enabled: true, URL: srv.URL, Timeout: time.Second})
	if _, err := rc.fetch(t.Context()); err == nil {
		t.Fatal("a non-200 response must be an error")
	}
}

func TestRemoteFetchRejectsGarbageBody(t *testing.T) {
	srv := newCountingServer(t, http.StatusOK, 0, "<html>not json</html>")
	rc := newRemote(&RemoteConfig{Enabled: true, URL: srv.URL, Timeout: time.Second})
	if _, err := rc.fetch(t.Context()); err == nil {
		t.Fatal("a non-JSON body must be an error")
	}
}

func TestBareModelID(t *testing.T) {
	cases := map[string]string{
		"a/b/c":     "c",
		"a/b":       "b",
		"solo":      "solo",
		"":          "",
		"trailing/": "trailing/",
		" openai/x": "x",
	}
	for in, want := range cases {
		if got := bareModelID(in); got != want {
			t.Errorf("bareModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanRemoteValue(t *testing.T) {
	if cleanRemoteValue(0) != 0 || cleanRemoteValue(-5) != 0 || cleanRemoteValue(remoteValueMax+1) != 0 {
		t.Fatal("dirty values must collapse to 0")
	}
	if cleanRemoteValue(4096) != 4096 {
		t.Fatal("a sane value must survive")
	}
}
