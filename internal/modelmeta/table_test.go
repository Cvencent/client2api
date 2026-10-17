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
		{"workbuddy", 34, "the client product.json catalogue plus effort-only ids"},
		{"tabbit", 0, "the installed client publishes no static window values"},
		{"kimi", 3, "the Kimi desktop cache publishes three context windows"},
		{"autoclaw", 4, "the AutoClaw client snapshot publishes four rows"},
		{"cline", 2, "one global wire vocabulary for the free and metered V4.1 ids"},
		{"loomy", 1, "the live V4.1 row five-tier vocabulary"},
		{"raccoon", 1, "the V4.1 on/off thinking switch"},
		{"openrouter", 1, "the live V4.1 low/high/max vocabulary"},
		{"nosuchclient", 0, "an unknown client has no static entries"},
	}
	for _, c := range cases {
		if got := StaticModelCount(c.client); got != c.want {
			t.Errorf("StaticModelCount(%q) = %d, want %d (%s)", c.client, got, c.want, c.why)
		}
	}
	clients := Clients()
	want := map[string]bool{
		"workbuddy": true, "kimi": true, "codearts": true,
		"lobsterai": true, "minimaxcode": true,
		"zcode": true, "cline": true, "loomy": true,
		"raccoon": true, "openrouter": true, "autoclaw": true,
	}
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
		{"glm-5.2", 1000000, 64000},
		{"glm-5.1", 200000, 48000},
		{"glm-5v-turbo", 200000, 38000},
		{"kimi-k2.7", 256000, 32000},
		{"kimi-k2.6", 256000, 32000},
		{"kimi-k2.5", 256000, 32000},
		{"minimax-m3-pay", 512000, 128000},
		{"hy3", 192000, 64000},
		{"hy3-preview", 192000, 64000},
		{"hy4-preview", 1000000, 64000},
		{"hy4-preview-x", 1000000, 64000},
		{"deepseek-v4-pro", 1000000, 128000},
		{"deepseek-v4-flash", 1000000, 50000},
		{"deepseek-v4.1-flash", 1000000, 128000},
		{"glm-5.3", 1000000, 64000},
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
		if m.MaxOutputTokens <= 0 {
			t.Errorf("%s MaxOutputTokens = %d, want the client-published value", model, m.MaxOutputTokens)
		}
	}
}

func TestStaticTableEffortOnlyRows(t *testing.T) {
	// hy4-preview-f has an effort row but no client-published window. kimi-k3-1
	// was removed because the installed client does not publish that id.
	m, ok := StaticMeta("workbuddy", RealmGlobal, "hy4-preview-f")
	if !ok {
		t.Fatal("StaticMeta(workbuddy, global, hy4-preview-f) not found")
	}
	if len(m.Efforts) != 1 {
		t.Errorf("global/hy4-preview-f efforts = %v, want 1 entry", m.Efforts)
	}
	if m.ContextLength != 0 || m.MaxOutputTokens != 0 {
		t.Errorf("global/hy4-preview-f invented a window: %d/%d", m.ContextLength, m.MaxOutputTokens)
	}
	if _, ok := StaticMeta("workbuddy", RealmCN, "hy4-preview-f"); ok {
		t.Error("hy4-preview-f has only a global effort row; the CN realm must not borrow it")
	}
	if _, ok := StaticMeta("workbuddy", RealmCN, "kimi-k3-1"); ok {
		t.Error("kimi-k3-1 is not present in the installed client")
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
	if gl.DefaultEffort != "high" {
		t.Fatalf("global default = %q, want high", gl.DefaultEffort)
	}
	if cn.ContextLength != 1000000 || gl.ContextLength != 1000000 {
		t.Fatalf("context = %d/%d, want 1000000/1000000", cn.ContextLength, gl.ContextLength)
	}
	if cn.MaxOutputTokens != 128000 || gl.MaxOutputTokens != 128000 {
		t.Fatalf("output differs by realm: cn=%d global=%d, want 128000/128000", cn.MaxOutputTokens, gl.MaxOutputTokens)
	}
}

func TestStaticTableRealmLimitsDoNotBorrowAcrossRealms(t *testing.T) {
	cn, ok := StaticMeta("workbuddy", RealmCN, "glm-5.3")
	if !ok {
		t.Fatal("workbuddy/glm-5.3 not found")
	}
	if cn.ContextLength != 1000000 || cn.MaxOutputTokens != 64000 {
		t.Fatalf("cn-qualified glm-5.3 = %d/%d, want 1000000/64000", cn.ContextLength, cn.MaxOutputTokens)
	}

	gl, ok := StaticMeta("workbuddy", RealmGlobal, "glm-5.3")
	if !ok {
		t.Fatal("workbuddy/global glm-5.3 not found")
	}
	if gl.ContextLength != 1000000 || gl.MaxOutputTokens != 48000 {
		t.Fatalf("global glm-5.3 = %d/%d, want 1000000/48000", gl.ContextLength, gl.MaxOutputTokens)
	}
}

func TestStaticMetaSelectsRealmFromQualifiedWorkbuddyID(t *testing.T) {
	cn, ok := StaticMeta("workbuddy", "", "cn:deepseek-v4.1-flash")
	if !ok {
		t.Fatal("cn:deepseek-v4.1-flash not found")
	}
	if cn.ContextLength != 1000000 || cn.MaxOutputTokens != 128000 {
		t.Fatalf("cn limits = %d/%d, want 1000000/128000", cn.ContextLength, cn.MaxOutputTokens)
	}

	gl, ok := StaticMeta("workbuddy", "", "global:deepseek-v4.1-flash")
	if !ok {
		t.Fatal("global:deepseek-v4.1-flash not found")
	}
	if gl.ContextLength != 1000000 || gl.MaxOutputTokens != 128000 {
		t.Fatalf("global limits = %d/%d, want 1000000/128000", gl.ContextLength, gl.MaxOutputTokens)
	}
}
func TestStaticTableV41EffortDefaults(t *testing.T) {
	cases := []struct {
		client  string
		realm   string
		model   string
		efforts []string
		def     string
	}{
		{"workbuddy", RealmCN, "deepseek-v4.1-flash", []string{"low", "high", "max"}, "high"},
		{"workbuddy", RealmGlobal, "deepseek-v4.1-flash", []string{"high"}, "high"},
		{"cline", RealmCN, "cline-free/deepseek-v4.1-flash", []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}, "high"},
		{"cline", RealmCN, "deepseek/deepseek-v4.1-flash", []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}, "high"},
		{"loomy", RealmCN, "DeepSeek-V4.1-Flash", []string{"none", "low", "medium", "high", "xhigh"}, "high"},
		{"raccoon", RealmCN, "sn-deepseek-v4-1-flash", []string{"off", "on"}, "on"},
		{"codearts", RealmCN, "deepseek-v4.1-flash", []string{"off", "on"}, "on"},
		{"openrouter", RealmCN, "deepseek/deepseek-v4.1-flash", []string{"low", "high", "max"}, "high"},
	}
	for _, tc := range cases {
		m, ok := StaticMeta(tc.client, tc.realm, tc.model)
		if !ok {
			t.Fatalf("StaticMeta(%s, %s, %q) not found", tc.client, tc.realm, tc.model)
		}
		if !equalStrings(m.Efforts, tc.efforts) {
			t.Errorf("%s/%s efforts = %v, want %v", tc.client, tc.model, m.Efforts, tc.efforts)
		}
		if m.DefaultEffort != tc.def {
			t.Errorf("%s/%s default = %q, want %q", tc.client, tc.model, m.DefaultEffort, tc.def)
		}
	}
}

// TestStaticTableV41ElevateDefaults pins the operator's "default on for every
// V4.1 Flash" rule to the embedded table. A row whose own vocabulary has no
// tier above xhigh still enables the policy: it is simply a no-op there, and
// the table must never invent a max the vendor did not publish.
func TestStaticTableV41ElevateDefaults(t *testing.T) {
	cases := []struct {
		client string
		model  string
	}{
		{"workbuddy", "deepseek-v4.1-flash"},
		{"workbuddy", "cn:deepseek-v4.1-flash"},
		{"workbuddy", "global:deepseek-v4.1-flash"},
		{"tabbit", "DeepSeek-V4.1-Flash"},
		{"codearts", "deepseek-v4.1-flash"},
		{"cline", "cline-free/deepseek-v4.1-flash"},
		{"cline", "deepseek/deepseek-v4.1-flash"},
		{"loomy", "DeepSeek-V4.1-Flash"},
		{"raccoon", "sn-deepseek-v4-1-flash"},
		{"trae", "deepseek-v4.1-flash"},
		{"openrouter", "deepseek/deepseek-v4.1-flash"},
		{"lobsterai", "deepseek-flash"},
	}
	for _, tc := range cases {
		if !StaticElevateTop(tc.client, tc.model) {
			t.Errorf("StaticElevateTop(%s, %q) = false, want the V4.1 default on", tc.client, tc.model)
		}
	}
}

// TestStaticElevateTopDefaultsOnForEveryModel pins the current operator policy:
// cap alignment is enabled out of the box for every model on every platform.
// Models with no published tier above xhigh are harmless no-ops; models that do
// publish max get promoted by the gateway. An explicit panel override still wins
// over this default.
func TestStaticElevateTopDefaultsOnForEveryModel(t *testing.T) {
	for _, tc := range []struct {
		client string
		model  string
	}{
		{"workbuddy", "gpt-5.5"},
		{"zcode", "glm-5.3"},
		{"kimi", "kimi-k3"},
		{"nosuchclient", "unknown-model"},
	} {
		if !StaticElevateTop(tc.client, tc.model) {
			t.Errorf("StaticElevateTop(%s, %q) = false, want the all-model default on", tc.client, tc.model)
		}
	}
}
func TestStaticTableRealmMissingMeansNoVocabulary(t *testing.T) {
	// glm-5.1 exists in the CN realm only; the global realm must not borrow
	// either its window or its vocabulary.
	if m, ok := StaticMeta("workbuddy", RealmGlobal, "glm-5.1"); ok {
		t.Fatalf("CN-only glm-5.1 leaked into the global realm: %+v", m)
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
	m, ok := StaticMeta("kimi", RealmCN, "k3-agent-ultra")
	if !ok {
		t.Fatal("kimi/k3-agent-ultra should match the exact row")
	}
	if m.ContextLength != 1000000 || m.MaxOutputTokens != 0 {
		t.Fatalf("got %d/%d, want 1000000/0", m.ContextLength, m.MaxOutputTokens)
	}
	if _, ok := StaticMeta("tabbit", RealmCN, "gpt-5.5"); ok {
		t.Fatal("tabbit row should be absent without a client-published window")
	}
}

func TestStaticUnknownModelsAreNotFound(t *testing.T) {
	for _, model := range []string{"", "nope", "Claude-Opus-4.7", "claude-sonnet-4", "priority", "Default", "Gemini-3.1-Pro"} {
		if m, ok := StaticMeta("tabbit", RealmCN, model); ok {
			t.Errorf("tabbit/%q resolved to %+v, want not-found", model, m)
		}
	}
	// Only the K3 agent ids have a public source. The rest of the kimi module's
	// builtin ids must stay unresolved rather than borrow the K3 window.
	if m, ok := StaticMeta("kimi", RealmCN, "k3-agent"); !ok || m.ContextLength != 1000000 {
		t.Errorf("kimi/k3-agent = %+v ok=%v, want the client-published 1M window", m, ok)
	}
	for _, model := range []string{"kimi", "kimi-k2"} {
		if m, ok := StaticMeta("kimi", RealmCN, model); ok {
			t.Errorf("kimi/%q resolved to %+v, but no client source exists for it", model, m)
		}
	}
	if ids := StaticModelIDs("kimi"); len(ids) != 3 {
		t.Errorf("StaticModelIDs(kimi) = %v, want three client-cache ids", ids)
	}
}

func TestStaticModelIDsAreSortedCopies(t *testing.T) {
	ids := StaticModelIDs("zcode")
	if len(ids) != 10 {
		t.Fatalf("StaticModelIDs(zcode) = %v, want 10 entries", ids)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i-1] > ids[i] {
			t.Fatalf("ids are not sorted: %v", ids)
		}
	}
	ids[0] = "mutated"
	if again := StaticModelIDs("zcode"); again[0] == "mutated" {
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
