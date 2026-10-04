package modelmeta

import (
	"encoding/json"
	"testing"
)

func TestStaticTableEntryCounts(t *testing.T) {
	cases := []struct {
		client string
		want   int
		why    string
	}{
		{"workbuddy", 33, "27 ids ported verbatim from model.json plus 6 effort-only ids"},
		{"tabbit", 4, "only ids whose name matches a reference table id"},
		{"kimi", 0, "no authoritative source names any of the kimi module's builtin ids"},
		{"nosuchclient", 0, "an unknown client has no static entries"},
	}
	for _, c := range cases {
		if got := StaticModelCount(c.client); got != c.want {
			t.Errorf("StaticModelCount(%q) = %d, want %d (%s)", c.client, got, c.want, c.why)
		}
	}
	clients := Clients()
	want := map[string]bool{"workbuddy": true, "tabbit": true}
	if len(clients) != len(want) {
		t.Fatalf("Clients() = %v, want %v", clients, want)
	}
	for _, c := range clients {
		if !want[c] {
			t.Fatalf("Clients() = %v, unexpected %q", clients, c)
		}
	}
}

func TestStaticTableWorkbuddyValuesAreVerbatim(t *testing.T) {
	cases := []struct {
		model   string
		context int64
		output  int64
	}{
		{"glm-5.2", 1000000, 131072},
		{"glm-5.1", 200000, 131072},
		{"glm-5v-turbo", 200000, 131072},
		{"kimi-k2.7", 256000, 65536},
		{"kimi-k2.6", 256000, 262144},
		{"kimi-k2.5", 164000, 262144},
		{"kimi-k3", 1048576, 131072},
		{"minimax-m3", 512000, 512000},
		{"hy3", 192000, 64000},
		{"hy3-preview", 262144, 64000},
		{"hy4-preview", 1000000, 64000},
		{"hy4-preview-x", 1000000, 64000},
		{"deepseek-v4-pro", 1000000, 384000},
		{"deepseek-v4-flash", 1000000, 384000},
		{"deepseek-v4.1-flash", 1000000, 384000},
		{"gpt-6-astra", 1050000, 128000},
		{"gpt-5.6-sol", 1050000, 128000},
		{"gpt-5.6-terra", 1050000, 128000},
		{"gpt-5.6-luna", 1050000, 128000},
		{"gpt-5.5", 1050000, 128000},
		{"gpt-5.4", 1050000, 128000},
		{"gpt-5.3-codex", 400000, 128000},
		{"gemini-3.5-flash", 1048576, 65536},
		{"glm-5.3", 1000000, 131072},
		{"glm-5.3-flash", 1000000, 131072},
	}
	for _, c := range cases {
		m, ok := StaticMeta("workbuddy", RealmCN, c.model)
		if !ok {
			t.Errorf("StaticMeta(workbuddy, cn, %q) not found", c.model)
			continue
		}
		if m.ContextLength != c.context || m.MaxOutputTokens != c.output {
			t.Errorf("%s = %d/%d, want %d/%d", c.model, m.ContextLength, m.MaxOutputTokens, c.context, c.output)
		}
		if m.Source != SourceStatic {
			t.Errorf("%s source = %q, want %q", c.model, m.Source, SourceStatic)
		}
	}
}

func TestStaticTableOmitsUnknownOutputLimits(t *testing.T) {
	// model.json has no max_output_tokens key for these ids. The reference omits the
	// field rather than guessing, and there is no safe direction for an output limit,
	// so the table must leave it empty.
	for _, model := range []string{"auto", "kimi-k2.8-preview"} {
		m, ok := StaticMeta("workbuddy", RealmCN, model)
		if !ok {
			t.Fatalf("StaticMeta(%q) not found", model)
		}
		if m.ContextLength <= 0 {
			t.Errorf("%s context = %d, want the sourced value", model, m.ContextLength)
		}
		if m.MaxOutputTokens != 0 {
			t.Errorf("%s MaxOutputTokens = %d, want 0 (no source exists)", model, m.MaxOutputTokens)
		}
		if m.SourceOf(FieldMaxOutputTokens) != "" {
			t.Errorf("%s output provenance = %q, want empty", model, m.SourceOf(FieldMaxOutputTokens))
		}
	}
}

func TestStaticTableEffortOnlyRows(t *testing.T) {
	// hy3-x, kimi-k3-1, hy4-preview-f, fast/balanced/primary-model appear in the
	// reference's effort tables but in no context table: efforts only, no window.
	for _, c := range []struct {
		model string
		realm string
		want  int
	}{
		{"hy3-x", RealmCN, 2},
		{"kimi-k3-1", RealmCN, 1},
		{"hy4-preview-f", RealmGlobal, 1},
		{"fast-model", RealmGlobal, 1},
		{"balanced-model", RealmGlobal, 1},
		{"primary-model", RealmGlobal, 1},
	} {
		m, ok := StaticMeta("workbuddy", c.realm, c.model)
		if !ok {
			t.Errorf("StaticMeta(workbuddy, %s, %q) not found", c.realm, c.model)
			continue
		}
		if len(m.Efforts) != c.want {
			t.Errorf("%s/%s efforts = %v, want %d entries", c.realm, c.model, m.Efforts, c.want)
		}
		if m.ContextLength != 0 || m.MaxOutputTokens != 0 {
			t.Errorf("%s/%s invented a window: %d/%d", c.realm, c.model, m.ContextLength, m.MaxOutputTokens)
		}
	}
	// And the same rows must not leak into the other realm.
	if _, ok := StaticMeta("workbuddy", RealmGlobal, "hy3-x"); ok {
		t.Error("hy3-x has only a CN effort row; the global realm must not borrow it")
	}
	if _, ok := StaticMeta("workbuddy", RealmCN, "fast-model"); ok {
		t.Error("fast-model has only a global effort row; the CN realm must not borrow it")
	}
}

func TestStaticTableEffortsAreRealmSplit(t *testing.T) {
	cn, ok := StaticMeta("workbuddy", RealmCN, "deepseek-v4.1-flash")
	if !ok {
		t.Fatal("deepseek-v4.1-flash not found in the CN table")
	}
	if len(cn.Efforts) != 3 {
		t.Fatalf("CN efforts = %v, want [low high max]", cn.Efforts)
	}
	if cn.DefaultEffort != "high" {
		t.Fatalf("CN default = %q, want high", cn.DefaultEffort)
	}
	gl, ok := StaticMeta("workbuddy", RealmGlobal, "deepseek-v4.1-flash")
	if !ok {
		t.Fatal("deepseek-v4.1-flash not found in the global table")
	}
	if len(gl.Efforts) != 1 || gl.Efforts[0] != "high" {
		t.Fatalf("global efforts = %v, want [high] (the international edition rejects low/max)", gl.Efforts)
	}
	if gl.DefaultEffort != "" {
		t.Fatalf("global default = %q, want empty: the reference sets no default there", gl.DefaultEffort)
	}
	// Context and output are realm-independent: same id, same window.
	if cn.ContextLength != gl.ContextLength || cn.ContextLength != 1000000 {
		t.Fatalf("context differs by realm: cn=%d global=%d", cn.ContextLength, gl.ContextLength)
	}
	if cn.MaxOutputTokens != gl.MaxOutputTokens || cn.MaxOutputTokens != 384000 {
		t.Fatalf("output differs by realm: cn=%d global=%d", cn.MaxOutputTokens, gl.MaxOutputTokens)
	}
}

func TestStaticTableRealmMissingMeansNoVocabulary(t *testing.T) {
	// glm-5.1 exists in the CN effort table only. The global realm still knows its
	// window (context is realm-independent) but must not be handed CN efforts.
	m, ok := StaticMeta("workbuddy", RealmGlobal, "glm-5.1")
	if !ok {
		t.Fatal("glm-5.1 should resolve globally for its window")
	}
	if len(m.Efforts) != 0 || m.DefaultEffort != "" {
		t.Fatalf("global efforts = %v default = %q, want none", m.Efforts, m.DefaultEffort)
	}
	if m.ContextLength != 200000 {
		t.Fatalf("context = %d, want 200000", m.ContextLength)
	}
	// An empty or unknown realm behaves like CN, matching the reference's realmKey.
	for _, realm := range []string{"", "cn", "CN", "  cn  ", "whatever"} {
		if got := NormalizeRealm(realm); got != RealmCN {
			t.Errorf("NormalizeRealm(%q) = %q, want %q", realm, got, RealmCN)
		}
	}
	for _, realm := range []string{"global", "Global", " GLOBAL "} {
		if got := NormalizeRealm(realm); got != RealmGlobal {
			t.Errorf("NormalizeRealm(%q) = %q, want %q", realm, got, RealmGlobal)
		}
	}
	cn, ok := StaticMeta("workbuddy", "whatever", "glm-5.1")
	if !ok || len(cn.Efforts) != 1 || cn.Efforts[0] != "medium" {
		t.Fatalf("an unknown realm should get the CN vocabulary, got %+v ok=%v", cn, ok)
	}
}

func TestStaticLookupFallsBackToCaseInsensitive(t *testing.T) {
	// tabbit lists its models capitalised while the reference table is lowercase.
	m, ok := StaticMeta("tabbit", RealmCN, "gpt-5.5")
	if !ok {
		t.Fatal("tabbit/GPT-5.5 (asked lowercase) should match the static row")
	}
	if m.ContextLength != 1050000 || m.MaxOutputTokens != 128000 {
		t.Fatalf("got %d/%d, want 1050000/128000", m.ContextLength, m.MaxOutputTokens)
	}
	if _, ok := StaticMeta("tabbit", RealmCN, "GPT-5.5"); !ok {
		t.Fatal("the exact id must resolve too")
	}
}

func TestStaticUnknownModelsAreNotFound(t *testing.T) {
	for _, model := range []string{"", "nope", "Claude-Opus-4.7", "claude-sonnet-4", "priority", "Default", "Gemini-3.1-Pro"} {
		if m, ok := StaticMeta("tabbit", RealmCN, model); ok {
			t.Errorf("tabbit/%q resolved to %+v, want not-found", model, m)
		}
	}
	// The kimi section is deliberately empty; see the note in table.json.
	for _, model := range []string{"kimi", "kimi-k2", "k3-agent", "k3-agent-ultra", "k2d6-agent"} {
		if m, ok := StaticMeta("kimi", RealmCN, model); ok {
			t.Errorf("kimi/%q resolved to %+v, but no source exists for it", model, m)
		}
	}
	if ids := StaticModelIDs("kimi"); len(ids) != 0 {
		t.Errorf("StaticModelIDs(kimi) = %v, want none", ids)
	}
}

func TestStaticModelIDsAreSortedCopies(t *testing.T) {
	ids := StaticModelIDs("tabbit")
	if len(ids) != 4 {
		t.Fatalf("StaticModelIDs(tabbit) = %v, want 4 entries", ids)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i-1] > ids[i] {
			t.Fatalf("ids are not sorted: %v", ids)
		}
	}
	ids[0] = "mutated"
	if again := StaticModelIDs("tabbit"); again[0] == "mutated" {
		t.Fatal("StaticModelIDs returned a view into package state")
	}
}

func TestParseStaticTableToleratesGarbage(t *testing.T) {
	for _, raw := range []string{"", "{", "[]", `{"_comment":["meta only"]}`, `{"a":{"models":"not an object"}}`} {
		st := parseStaticTable([]byte(raw))
		if st == nil {
			t.Fatalf("parseStaticTable(%q) returned nil", raw)
		}
		if _, ok := st.entry("a", "b"); ok {
			t.Fatalf("parseStaticTable(%q) produced a phantom entry", raw)
		}
	}
}

func TestParseStaticTableReadsAMinimalDoc(t *testing.T) {
	st := parseStaticTable([]byte(`{"_comment":["x"],"demo":{"models":{
		"m1":{"context_length":10,"max_output_tokens":2},
		"m2":{"efforts_by_realm":{"cn":{"efforts":["high"],"default_effort":"high"},"global":{"efforts":["low"]}}},
		"m3":{}}}}`))
	if got := st.counts["demo"]; got != 3 {
		t.Fatalf("counts[demo] = %d, want 3", got)
	}
	e, ok := st.entry("demo", "m1")
	if !ok || e.ContextLength != 10 || e.MaxOutputTokens != 2 {
		t.Fatalf("m1 = %+v ok=%v", e, ok)
	}
	cn := e0Meta(t, st, "demo", "m2", RealmCN)
	if cn.DefaultEffort != "high" {
		t.Fatalf("m2 cn default = %q, want high", cn.DefaultEffort)
	}
	gl := e0Meta(t, st, "demo", "m2", RealmGlobal)
	if len(gl.Efforts) != 1 || gl.Efforts[0] != "low" || gl.DefaultEffort != "" {
		t.Fatalf("m2 global = %+v, want [low] and no default", gl)
	}
	// A row with no fields at all is a not-found, not a zero-value lie.
	e3, _ := st.entry("demo", "m3")
	if m := e3.meta(RealmCN); !m.IsZero() {
		t.Fatalf("an empty row produced %+v", m)
	}
}

func e0Meta(t *testing.T, st *staticTable, client, model, realm string) Meta {
	t.Helper()
	e, ok := st.entry(client, model)
	if !ok {
		t.Fatalf("entry(%q, %q) missing", client, model)
	}
	return e.meta(realm)
}

func TestStaticTableSectionsAreIndependent(t *testing.T) {
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(tableJSON, &sections); err != nil {
		t.Fatalf("the embedded table is not a JSON object: %v", err)
	}
	if len(sections) == 0 {
		t.Fatal("the embedded table is empty")
	}
}
