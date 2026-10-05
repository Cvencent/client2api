package modelmeta

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestProviderUnknownModelIsNotFoundNotZero(t *testing.T) {
	p := New(Options{Client: "workbuddy"})
	m, ok := p.Lookup("definitely-not-a-real-model")
	if ok {
		t.Fatalf("Lookup returned %+v for an unknown model", m)
	}
	if !m.IsZero() {
		t.Fatalf("the not-found Meta is not zero: %+v", m)
	}
	if _, ok := p.Lookup(""); ok {
		t.Fatal("an empty model id must not resolve")
	}
}

func TestProviderStaticTableServesWorkbuddy(t *testing.T) {
	p := New(Options{Client: "workbuddy"})
	m, ok := p.Lookup("glm-5.2")
	if !ok {
		t.Fatal("glm-5.2 is in the ported reference table")
	}
	if m.ContextLength != 1000000 || m.MaxOutputTokens != 131072 {
		t.Fatalf("glm-5.2 = %d/%d, want 1000000/131072", m.ContextLength, m.MaxOutputTokens)
	}
	if m.Source != SourceStatic || m.SourceOf(FieldContextLength) != SourceStatic {
		t.Fatalf("provenance = %q/%q", m.Source, m.SourceOf(FieldContextLength))
	}
	// A model whose reference entry has no max_output_tokens must omit the field
	// rather than guess an output limit.
	k, ok := p.Lookup("kimi-k2.8-preview")
	if !ok {
		t.Fatal("kimi-k2.8-preview is in the table")
	}
	if k.ContextLength != 1048576 {
		t.Fatalf("context = %d", k.ContextLength)
	}
	if k.Has(FieldMaxOutputTokens) {
		t.Fatalf("max_output_tokens = %d, want the field omitted (the reference has no value for it)", k.MaxOutputTokens)
	}
}

func TestProviderVendorBeatsEveryFallback(t *testing.T) {
	srv := newCountingServer(t, 200, 0, testModelsDevDoc)
	dir := t.TempDir()
	p := New(Options{
		Client:   "workbuddy",
		CacheDir: dir,
		Remote:   &RemoteConfig{Enabled: true, URL: srv.URL, Cooldown: time.Hour},
	})
	// Seed the cache with a stale value so all three fallbacks disagree with the
	// vendor, and with each other.
	p.Store().Put("glm-5.2", Meta{ContextLength: 3, MaxOutputTokens: 4, Efforts: []string{"bogus"}})

	vendor := Meta{ContextLength: 4242, Source: SourceVendor}
	got := p.Merge("glm-5.2", vendor)
	if got.ContextLength != 4242 {
		t.Fatalf("context = %d, want the vendor's 4242", got.ContextLength)
	}
	if got.SourceOf(FieldContextLength) != SourceVendor {
		t.Fatalf("context provenance = %q, want vendor", got.SourceOf(FieldContextLength))
	}
	// The static table may still fill the fields the vendor left empty.
	if got.MaxOutputTokens != 131072 {
		t.Fatalf("max_output_tokens = %d, want the static 131072", got.MaxOutputTokens)
	}
	if got.SourceOf(FieldMaxOutputTokens) != SourceStatic {
		t.Fatalf("max_output_tokens provenance = %q, want static", got.SourceOf(FieldMaxOutputTokens))
	}
}

func TestProviderVendorEffortsAreAuthoritative(t *testing.T) {
	p := New(Options{Client: "workbuddy"})
	// deepseek-v4-flash has a static CN effort vocabulary; a live value must win
	// outright rather than being unioned or replaced.
	got := p.Merge("deepseek-v4-flash", Meta{Efforts: []string{"medium"}, Source: SourceVendor})
	if len(got.Efforts) != 1 || got.Efforts[0] != "medium" {
		t.Fatalf("efforts = %v, want the vendor's [medium]", got.Efforts)
	}
	if got.DefaultEffort != "" {
		t.Fatalf("default = %q, want empty: the vendor named no default", got.DefaultEffort)
	}
	// With nothing from the vendor, the static vocabulary applies.
	got = p.Merge("deepseek-v4-flash", Meta{})
	if want := []string{"low", "high", "max"}; !equalStrings(got.Efforts, want) {
		t.Fatalf("efforts = %v, want %v", got.Efforts, want)
	}
}

func TestProviderStaticBeatsRemote(t *testing.T) {
	srv := newCountingServer(t, 200, 0, testModelsDevDoc)
	p := New(Options{
		Client: "workbuddy",
		Remote: &RemoteConfig{Enabled: true, URL: srv.URL, Cooldown: time.Hour},
	})
	// "glm-5.2" is in both the reference table (1000000/131072) and the fake
	// models.dev document (5/5).
	if _, ok := p.Lookup("glm-5.2"); !ok {
		t.Fatal("the static table must answer before any network access")
	}
	if !p.WaitRemote(5 * time.Second) {
		t.Fatal("the refresh never landed")
	}
	m, ok := p.Lookup("glm-5.2")
	if !ok {
		t.Fatal("glm-5.2 vanished")
	}
	if m.ContextLength != 1000000 || m.MaxOutputTokens != 131072 {
		t.Fatalf("got %d/%d, want the static 1000000/131072 (static beats models.dev)", m.ContextLength, m.MaxOutputTokens)
	}
	if m.SourceOf(FieldContextLength) != SourceStatic {
		t.Fatalf("provenance = %q, want static", m.SourceOf(FieldContextLength))
	}
	// "acme-only" is only in the document, so it does resolve.
	if _, ok := p.Lookup("acme-only"); !ok {
		t.Fatal("a models.dev-only model should resolve once the doc is loaded")
	}
}

func TestProviderRemoteBeatsCache(t *testing.T) {
	srv := newCountingServer(t, 200, 0, testModelsDevDoc)
	dir := t.TempDir()
	p := New(Options{
		Client:   "workbuddy",
		CacheDir: dir,
		Remote:   &RemoteConfig{Enabled: true, URL: srv.URL, Cooldown: time.Hour},
	})
	p.Store().Put("acme-only", Meta{ContextLength: 999, MaxOutputTokens: 998, Source: SourceStatic})

	if m, ok := p.Lookup("acme-only"); !ok || m.ContextLength != 999 {
		t.Fatalf("before the refresh the cache should answer: %+v ok=%v", m, ok)
	}
	if !p.WaitRemote(5 * time.Second) {
		t.Fatal("the refresh never landed")
	}
	m, ok := p.Lookup("acme-only")
	if !ok {
		t.Fatal("acme-only vanished")
	}
	if m.ContextLength != 4096 || m.MaxOutputTokens != 512 {
		t.Fatalf("got %d/%d, want the models.dev 4096/512 (remote beats cache)", m.ContextLength, m.MaxOutputTokens)
	}
	if m.SourceOf(FieldContextLength) != SourceModelsDev {
		t.Fatalf("provenance = %q, want models-dev", m.SourceOf(FieldContextLength))
	}
}

func TestProviderCacheIsTheLastResortAndOnlyForItsModel(t *testing.T) {
	dir := t.TempDir()
	p := New(Options{Client: "workbuddy", CacheDir: dir})
	if p.Store() == nil {
		t.Fatal("CacheDir should have produced a store")
	}
	p.Store().Put("recovered-model", Meta{ContextLength: 123456, MaxOutputTokens: 789, Source: SourceStatic})

	m, ok := p.Lookup("recovered-model")
	if !ok {
		t.Fatal("the cache did not answer")
	}
	if m.ContextLength != 123456 || m.MaxOutputTokens != 789 {
		t.Fatalf("cache value = %d/%d", m.ContextLength, m.MaxOutputTokens)
	}
	if m.Source != SourceCache || m.SourceOf(FieldContextLength) != SourceCache {
		t.Fatalf("provenance = %q/%q, want cache", m.Source, m.SourceOf(FieldContextLength))
	}
	// A cached value must never leak to another id.
	if got, ok := p.Lookup("recovered-mode"); ok {
		t.Fatalf("a near-miss id resolved from the cache: %+v", got)
	}
}

func TestProviderCorruptCacheDoesNotPoisonTheAnswer(t *testing.T) {
	dir := t.TempDir()
	p := New(Options{Client: "workbuddy", CacheDir: dir, Logf: t.Logf})
	if err := os.WriteFile(p.Store().Path(), []byte("not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, ok := p.Lookup("glm-5.2")
	if !ok || m.ContextLength != 1000000 {
		t.Fatalf("a corrupt cache broke the static answer: %+v ok=%v", m, ok)
	}
	if _, ok := p.Lookup("nope"); ok {
		t.Fatal("a corrupt cache produced a value for an unknown model")
	}
}

func TestProviderContextCapIsOptIn(t *testing.T) {
	off := New(Options{Client: "workbuddy"})
	if m, ok := off.Lookup("no-such-model"); ok {
		t.Fatalf("without a cap an unknown model must be not-found, got %+v", m)
	}

	on := New(Options{Client: "workbuddy", ContextCap: DefaultContextWindow})
	m, ok := on.Lookup("no-such-model")
	if !ok {
		t.Fatal("with a cap the model should resolve at the policy value")
	}
	if m.ContextLength != DefaultContextWindow {
		t.Fatalf("context = %d, want %d", m.ContextLength, DefaultContextWindow)
	}
	if m.SourceOf(FieldContextLength) != SourceFallback {
		t.Fatalf("provenance = %q, want %q", m.SourceOf(FieldContextLength), SourceFallback)
	}
	if m.Has(FieldMaxOutputTokens) {
		t.Fatalf("the cap must never invent an output limit, got %d", m.MaxOutputTokens)
	}
	// A known model is unaffected by the cap.
	if k, ok := on.Lookup("glm-5.1"); !ok || k.ContextLength != 200000 {
		t.Fatalf("cap overrode a known value: %+v ok=%v", k, ok)
	}
	if k, _ := on.Lookup("glm-5.1"); k.SourceOf(FieldContextLength) != SourceStatic {
		t.Fatalf("provenance = %q, want static", k.SourceOf(FieldContextLength))
	}
}

func TestProviderRealmSplit(t *testing.T) {
	cn := New(Options{Client: "workbuddy"})
	if cn.Realm() != RealmCN {
		t.Fatalf("Realm() = %q, want %q for an empty realm", cn.Realm(), RealmCN)
	}
	m, ok := cn.Lookup("deepseek-v4.1-flash")
	if !ok {
		t.Fatal("deepseek-v4.1-flash is in the table")
	}
	if want := []string{"low", "high", "max"}; !equalStrings(m.Efforts, want) {
		t.Fatalf("CN efforts = %v, want %v", m.Efforts, want)
	}
	if m.DefaultEffort != "high" {
		t.Fatalf("CN default = %q, want high", m.DefaultEffort)
	}

	gl := New(Options{Client: "workbuddy", Realm: "global"})
	if gl.Realm() != RealmGlobal {
		t.Fatalf("Realm() = %q", gl.Realm())
	}
	g, ok := gl.Lookup("deepseek-v4.1-flash")
	if !ok {
		t.Fatal("deepseek-v4.1-flash is in the global table too")
	}
	if want := []string{"high"}; !equalStrings(g.Efforts, want) {
		t.Fatalf("global efforts = %v, want %v (the international edition rejects low/max)", g.Efforts, want)
	}
	if g.DefaultEffort != "" {
		t.Fatalf("global default = %q, want empty", g.DefaultEffort)
	}
	// Context is realm-independent.
	if g.ContextLength != m.ContextLength || g.ContextLength != 1000000 {
		t.Fatalf("context differs by realm: %d vs %d", g.ContextLength, m.ContextLength)
	}
	// An odd-case realm string still maps to the CN table, like the reference.
	if r := New(Options{Client: "workbuddy", Realm: "GLOBAL"}); r.Realm() != RealmGlobal {
		t.Fatalf("realm normalisation = %q", r.Realm())
	}
	if r := New(Options{Client: "workbuddy", Realm: "cn"}); r.Realm() != RealmCN {
		t.Fatalf("realm normalisation = %q", r.Realm())
	}
}

func TestProviderGlobalOnlyModelIsAbsentInCN(t *testing.T) {
	cn := New(Options{Client: "workbuddy"})
	gl := New(Options{Client: "workbuddy", Realm: RealmGlobal})
	// hy4-preview-f is a global-only effort row.
	if m, ok := gl.Lookup("hy4-preview-f"); !ok || !equalStrings(m.Efforts, []string{"high"}) {
		t.Fatalf("global hy4-preview-f = %+v ok=%v", m, ok)
	}
	if m, ok := cn.Lookup("hy4-preview-f"); ok {
		t.Fatalf("a global-only row leaked into CN: %+v", m)
	}
	// fast-model is global-only and has no default.
	if m, ok := gl.Lookup("fast-model"); !ok || m.DefaultEffort != "" {
		t.Fatalf("fast-model = %+v ok=%v", m, ok)
	}
}

func TestProviderKimiAndTabbitHaveNoInventedNumbers(t *testing.T) {
	kimi := New(Options{Client: "kimi"})
	// k3-agent/k3-agent-ultra carry a documented Kimi K3 window; the remaining
	// builtin ids have no public source and must stay unresolved.
	for _, id := range []string{"kimi", "kimi-k2", "k2d6-agent"} {
		if m, ok := kimi.Lookup(id); ok {
			t.Errorf("kimi %q resolved to %+v; no authoritative source covers this id", id, m)
		}
	}

	tabbit := New(Options{Client: "tabbit"})
	m, ok := tabbit.Lookup("gpt-5.5")
	if !ok {
		t.Fatal("tabbit gpt-5.5 is sourced from the reference")
	}
	if m.ContextLength != 1050000 || m.MaxOutputTokens != 128000 {
		t.Fatalf("tabbit gpt-5.5 = %d/%d", m.ContextLength, m.MaxOutputTokens)
	}
	// Case-insensitive lookup, because tabbit's ids are display-cased.
	if up, ok := tabbit.Lookup("GPT-5.5"); !ok || up.ContextLength != m.ContextLength {
		t.Fatalf("case-insensitive lookup failed: %+v ok=%v", up, ok)
	}
	for _, id := range []string{"Claude-Opus-4.7", "Claude-Sonnet-4.6", "Gemini-3.1-Pro", "priority", "Default"} {
		if m, ok := tabbit.Lookup(id); ok {
			t.Errorf("tabbit %q resolved to %+v; that id has no sourced numbers", id, m)
		}
	}
}

func TestProviderNeverContactsTheNetworkUnprompted(t *testing.T) {
	// A provider built the way a wired module builds it — no Remote at all — must
	// be entirely offline, which is why the default path is testable without a
	// network and cannot break a user's laptop.
	p := New(Options{Client: "workbuddy", ContextCap: DefaultContextWindow})
	if p.RemoteEnabled() {
		t.Fatal("RemoteEnabled() = true without an opt-in")
	}
	if p.WaitRemote(time.Millisecond) {
		t.Fatal("WaitRemote reported a document without an opt-in")
	}
}

func TestProviderWarmsTheCacheWithTheMergedAnswer(t *testing.T) {
	dir := t.TempDir()
	first := New(Options{Client: "workbuddy", CacheDir: dir})
	m, ok := first.Lookup("glm-5.3")
	if !ok || m.ContextLength != 1000000 {
		t.Fatalf("static lookup failed: %+v ok=%v", m, ok)
	}
	// The next cold start, with no static entry and no vendor, still has the value.
	p := New(Options{Client: "kimi", CacheDir: dir})
	if m, ok := p.Lookup("glm-5.3"); ok {
		t.Fatalf("kimi resolved a workbuddy value: %+v", m)
	}
	raw, err := os.ReadFile(first.Store().Path())
	if err != nil {
		t.Fatalf("the merged answer was not persisted: %v", err)
	}
	var doc cacheDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the cache file is not valid JSON: %v", err)
	}
	if doc.Client != "workbuddy" {
		t.Fatalf("cache client = %q", doc.Client)
	}
	if e, ok := doc.Models["glm-5.3"]; !ok || e.ContextLength != 1000000 {
		t.Fatalf("cache entry = %+v ok=%v", e, ok)
	}
	if e := doc.Models["glm-5.3"]; e.Source != SourceStatic {
		t.Fatalf("cache entry source = %q, want the pre-cache provenance", e.Source)
	}
}

func TestProviderMergeRetainsVendorProvenance(t *testing.T) {
	p := New(Options{Client: "workbuddy"})
	got := p.Merge("glm-5.1", Meta{ContextLength: 200000, Source: SourceVendor})
	if got.SourceOf(FieldContextLength) != SourceVendor {
		t.Fatalf("context provenance = %q, want vendor", got.SourceOf(FieldContextLength))
	}
	if got.SourceOf(FieldMaxOutputTokens) != SourceStatic {
		t.Fatalf("output provenance = %q, want static", got.SourceOf(FieldMaxOutputTokens))
	}
	if got.Source != SourceVendor {
		t.Fatalf("aggregate Source = %q, want the strongest source (vendor)", got.Source)
	}
	// An unlabelled vendor value is treated as vendor provenance.
	got = p.Merge("glm-5.1", Meta{ContextLength: 1})
	if got.SourceOf(FieldContextLength) != SourceVendor {
		t.Fatalf("unlabelled vendor provenance = %q", got.SourceOf(FieldContextLength))
	}
	// A zero vendor Meta changes nothing and must not mark anything as vendor.
	got = p.Merge("glm-5.1", Meta{})
	if got.SourceOf(FieldContextLength) != SourceStatic {
		t.Fatalf("provenance = %q, want static", got.SourceOf(FieldContextLength))
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
