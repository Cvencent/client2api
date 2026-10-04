package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Prompts
// ---------------------------------------------------------------------------

func TestLongPromptTargetScalesWithTheRung(t *testing.T) {
	cases := []struct {
		maxTokens int
		want      string
		why       string
	}{
		{30000, "10000", "a third of the rung leaves headroom before the upstream runs out of budget"},
		{6000, "2000", "the floor keeps a small rung from asking for a trivially short answer"},
		{600, "2000", "the floor also applies when the rung itself is tiny"},
		{900000, "200000", "the ceiling keeps the target from becoming an impossible task"},
	}
	for _, c := range cases {
		got := longPrompt(c.maxTokens)
		if !strings.Contains(got, c.want) {
			t.Errorf("longPrompt(%d) = %q, want it to name %s (%s)", c.maxTokens, got, c.want, c.why)
		}
		// The prompt has to forbid summarising, or the model answers briefly
		// and the rung measures the model's patience instead of the ceiling.
		for _, marker := range []string{"每行一个", "不要总结", "不要提前结束"} {
			if !strings.Contains(got, marker) {
				t.Errorf("longPrompt(%d) dropped the %q instruction", c.maxTokens, marker)
			}
		}
	}
}

func TestLongPromptHardSharesTheTargetButChangesTheWording(t *testing.T) {
	softRe := regexp.MustCompile(`一直数到 (\d+)`)
	hardRe := regexp.MustCompile(`Count from 1 to (\d+),`)
	for _, rung := range []int{600, 6000, 30000, 900000} {
		soft, hard := longPrompt(rung), longPromptHard(rung)
		// Same target, different sentence: the retry exists because the model
		// ignored the first wording, so re-sending it verbatim would be useless.
		if soft == hard {
			t.Errorf("rung %d: the retry prompt is identical to the first", rung)
		}
		for _, want := range []string{"Count from 1 to", "one number per line", "do not stop early"} {
			if !strings.Contains(hard, want) {
				t.Errorf("longPromptHard(%d) = %q, want it to contain %q", rung, hard, want)
			}
		}
		// The target is computed identically in both wordings, so the retry
		// measures the same ceiling the first attempt was aiming at.
		sm, hm := softRe.FindStringSubmatch(soft), hardRe.FindStringSubmatch(hard)
		if sm == nil || hm == nil {
			t.Fatalf("rung %d: could not read the target back (soft=%q hard=%q)", rung, soft, hard)
		}
		if sm[1] != hm[1] {
			t.Errorf("rung %d: soft target %s but hard target %s", rung, sm[1], hm[1])
		}
	}
}

// ---------------------------------------------------------------------------
// Keyword matching
// ---------------------------------------------------------------------------

func TestMatchScorePrefersTheNamespacedExactMatch(t *testing.T) {
	cases := []struct {
		id       string
		keywords []string
		want     int
	}{
		{"cn:glm-5.2", []string{"cn:glm-5.2"}, 3},
		// The whole reason the namespaced form scores 3: a bare substring match
		// would let --models cn:glm-5.2 also select the global realm's model.
		{"global:glm-5.2", []string{"cn:glm-5.2"}, 0},
		{"cn:glm-5.2", []string{"glm-5.2"}, 2},
		{"global:glm-5.2", []string{"glm-5.2"}, 2},
		{"cn:glm-5.2-air", []string{"glm-5.2"}, 1},
		{"cn:other", []string{"glm-5.2"}, 0},
		{"CN:GLM-5.2", []string{"cn:glm-5.2"}, 3},
	}
	for _, c := range cases {
		if got := matchScore(c.id, c.keywords); got != c.want {
			t.Errorf("matchScore(%q, %v) = %d, want %d", c.id, c.keywords, got, c.want)
		}
	}
}

func TestParseTiersDropsJunkAndSorts(t *testing.T) {
	if got := fmt.Sprint(parseTiers("100000, 40000 ,,x,0,-5,70000")); got != "[40000 70000 100000]" {
		t.Errorf("parseTiers = %s, want [40000 70000 100000]", got)
	}
	if got := parseTiers(""); len(got) != 0 {
		t.Errorf("parseTiers(\"\") = %v, want empty", got)
	}
}

func TestSplitKeywordsLowercasesAndTrims(t *testing.T) {
	if got := fmt.Sprint(splitKeywords(" GLM-5.2 , ,cn:GLM ")); got != "[glm-5.2 cn:glm]" {
		t.Errorf("splitKeywords = %s", got)
	}
}

// ---------------------------------------------------------------------------
// Classification into the panel vocabulary
// ---------------------------------------------------------------------------

func TestClassifyMapsEveryVerdictOntoThePanelVocabulary(t *testing.T) {
	n := func(v int) *int { return &v }
	cases := []struct {
		name        string
		res         probeResult
		wantVerdict string
		wantMeas    *int
	}{
		{
			name:        "a bare clamp",
			res:         probeResult{Verdict: "钳制", Measured: n(48000)},
			wantVerdict: "clamped", wantMeas: n(48000),
		},
		{
			name:        "a clamp bracketed by the previous full rung",
			res:         probeResult{Verdict: "≈48000（钳制；上一档满额 40000，取值 48000）", Measured: n(48000)},
			wantVerdict: "clamped", wantMeas: n(48000),
		},
		{
			name:        "the model stopped on its own, so the floor is the fullest rung",
			res:         probeResult{Verdict: "≥40000（模型主动收尾，未测到钳制点；声称 131072）", MaxFull: 40000},
			wantVerdict: "at_least", wantMeas: n(40000),
		},
		{
			name:        "the top rung still came back full",
			res:         probeResult{Verdict: "≥100000（最高档位仍满额，未触顶）", Measured: n(100000), MaxFull: 100000},
			wantVerdict: "at_least", wantMeas: n(100000),
		},
		{
			name:        "the budget ran out after a full rung",
			res:         probeResult{Verdict: "未测完（超出时间预算 600s；已知至少 >40000）", MaxFull: 40000},
			wantVerdict: "at_least", wantMeas: n(40000),
		},
		{
			// The bound in the text is the only number available; recovering it
			// is the difference between a usable floor and "no idea".
			name:        "the budget ran out before anything was measured",
			res:         probeResult{Verdict: "未测完（超出时间预算 600s；已知至少 >0）"},
			wantVerdict: "inconclusive", wantMeas: nil,
		},
		{
			name:        "the model stopped and nothing was ever full",
			res:         probeResult{Verdict: "模型主动停止（未能测出上限，非钳制）"},
			wantVerdict: "inconclusive", wantMeas: nil,
		},
		{
			name:        "every request failed",
			res:         probeResult{Verdict: "全部请求失败（模型/账号不可用，无结论）"},
			wantVerdict: "inconclusive", wantMeas: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotVerdict, gotMeas := classify(c.res)
			if gotVerdict != c.wantVerdict {
				t.Errorf("verdict = %q, want %q", gotVerdict, c.wantVerdict)
			}
			switch {
			case c.wantMeas == nil && gotMeas != nil:
				t.Errorf("measured = %d, want none", *gotMeas)
			case c.wantMeas != nil && gotMeas == nil:
				t.Errorf("measured = none, want %d", *c.wantMeas)
			case c.wantMeas != nil && *gotMeas != *c.wantMeas:
				t.Errorf("measured = %d, want %d", *gotMeas, *c.wantMeas)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The panel contract file
// ---------------------------------------------------------------------------

func readContract(t *testing.T, path string) map[string]probeRecord {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var doc struct {
		Version int                    `json:"version"`
		Probes  map[string]probeRecord `json:"probes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing %s: %v\n%s", path, err, raw)
	}
	if doc.Version != 1 {
		t.Errorf("version = %d, want 1", doc.Version)
	}
	return doc.Probes
}

func TestWritePanelOutMergesAndKeepsUntestedModels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output_probes.json")

	// A previous run left two records behind.
	seed := `{"version":1,"probes":{` +
		`"cn:glm-5.2":{"claimed":131072,"measured":48000,"verdict":"clamped","note":"old","tested_at":"2026-01-01 00:00:00","source":"cmd/probe"},` +
		`"cn:glm-4.7":{"claimed":65536,"measured":null,"verdict":"inconclusive","note":"kept","tested_at":"2026-01-01 00:00:00","source":"cmd/probe"}}}`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	measured := 32000
	claimed := 131072
	n, err := writePanelOut(path, []probeResult{{
		Model:         "cn:glm-5.2",
		Measured:      &measured,
		Verdict:       "≈32000（钳制；上一档满额 40000，取值 32000）",
		ClaimedOutput: &claimed,
		Time:          "2026-02-02 12:00:00",
	}})
	if err != nil {
		t.Fatalf("writePanelOut: %v", err)
	}
	if n != 2 {
		t.Errorf("reported %d probes, want 2 (the retested one plus the untouched one)", n)
	}

	probes := readContract(t, path)
	if len(probes) != 2 {
		t.Fatalf("file holds %d probes, want 2: %v", len(probes), probes)
	}
	// The model that was not retested must survive untouched — that is the whole
	// point of merging: a one-model run must not wipe the rest of the file.
	if got := probes["cn:glm-4.7"]; got.Note != "kept" || got.Verdict != "inconclusive" {
		t.Errorf("the untouched probe was rewritten: %+v", got)
	}
	got := probes["cn:glm-5.2"]
	if got.Verdict != "clamped" {
		t.Errorf("verdict = %q, want clamped", got.Verdict)
	}
	if got.Measured == nil || *got.Measured != 32000 {
		t.Errorf("measured = %v, want 32000", got.Measured)
	}
	if got.Claimed == nil || *got.Claimed != 131072 {
		t.Errorf("claimed = %v, want 131072", got.Claimed)
	}
	// The human-readable note is the raw verdict, which is what the panel shows
	// in the tooltip; losing it would leave only the three-state label.
	if !strings.Contains(got.Note, "上一档满额 40000") {
		t.Errorf("note = %q, want the original verdict text", got.Note)
	}
	if got.Source != probeSource {
		t.Errorf("source = %q, want %q", got.Source, probeSource)
	}
	if got.TestedAt != "2026-02-02 12:00:00" {
		t.Errorf("tested_at = %q", got.TestedAt)
	}
}

func TestWritePanelOutRebuildsACorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output_probes.json")
	if err := os.WriteFile(path, []byte("{not json at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	measured := 1000
	if _, err := writePanelOut(path, []probeResult{{Model: "cn:x", Measured: &measured, Verdict: "钳制"}}); err != nil {
		t.Fatalf("a corrupt cache must not block the run that just paid for the measurement: %v", err)
	}
	probes := readContract(t, path)
	if len(probes) != 1 || probes["cn:x"].Measured == nil || *probes["cn:x"].Measured != 1000 {
		t.Errorf("rebuilt file = %v", probes)
	}
}

func TestWritePanelOutLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output_probes.json")
	if _, err := writePanelOut(path, nil); err != nil {
		t.Fatal(err)
	}
	// tmp + rename is what keeps a panel query concurrent with a probe run from
	// ever reading a half-written document.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("a temporary file was left behind: %v", err)
	}
	probes := readContract(t, path)
	if probes == nil {
		t.Error("an empty run should still write a valid document with an empty probes map")
	}
}

func TestWritePanelOutCreatesTheDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "nested", "output_probes.json")
	if _, err := writePanelOut(path, nil); err != nil {
		t.Fatalf("writePanelOut should create the directory: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file was not created: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The streaming reader
// ---------------------------------------------------------------------------

// sseServer answers /chat/completions with the frames a probe run cares about:
// a content delta, then the terminal frame carrying usage and finish_reason.
func sseServer(t *testing.T, finish string, out, reasoning int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"1\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":%q}],\"usage\":{\"completion_tokens\":%d,\"completion_tokens_details\":{\"reasoning_tokens\":%d}}}\n\n",
			finish, out, reasoning)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func TestProbeOnceReadsTheUsageAndFinishReason(t *testing.T) {
	srv := sseServer(t, "length", 48000, 120)
	defer srv.Close()

	sample, err := probeOnce(srv.Client(), srv.URL+"/v1", "k", "cn:m", 100000, "count")
	if err != nil {
		t.Fatalf("probeOnce: %v", err)
	}
	if !sample.HasOut || sample.Out != 48000 {
		t.Errorf("out = %d (has=%v), want 48000", sample.Out, sample.HasOut)
	}
	if sample.Reasoning != 120 {
		t.Errorf("reasoning = %d, want 120", sample.Reasoning)
	}
	if sample.Finish != "length" {
		t.Errorf("finish = %q, want length", sample.Finish)
	}
}

func TestProbeOnceDistinguishesNoUsageFromZeroTokens(t *testing.T) {
	// A stream that ends without a usage block: the ladder must not read this as
	// "the model produced zero tokens", because that would look like a clamp.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"1\"},\"finish_reason\":\"length\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	sample, err := probeOnce(srv.Client(), srv.URL+"/v1", "k", "cn:m", 100, "count")
	if err != nil {
		t.Fatalf("probeOnce: %v", err)
	}
	if sample.HasOut {
		t.Errorf("HasOut = true for a stream with no usage block (out=%d)", sample.Out)
	}
	if sample.Finish != "length" {
		t.Errorf("finish = %q, want length", sample.Finish)
	}
}

// TestProbeOnceAsksForTheUsageFrame pins the request shape: the ladder reads its
// answer out of the final usage frame, and an OpenAI-compatible gateway may send
// that frame only when the request asks for it.  This gateway does, so a probe
// that omits stream_options measures nothing against it.
func TestProbeOnceAsksForTheUsageFrame(t *testing.T) {
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		sent = body
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	if _, err := probeOnce(srv.Client(), srv.URL+"/v1", "k", "cn:m", 100, "count"); err != nil {
		t.Fatalf("probeOnce: %v", err)
	}
	opts, _ := sent["stream_options"].(map[string]any)
	if opts == nil {
		t.Fatalf("the request carried no stream_options: %v", sent)
	}
	if include, _ := opts["include_usage"].(bool); !include {
		t.Errorf("stream_options = %v, want include_usage true", opts)
	}
}

// TestProbeModelNeverLeavesTheVerdictBlank pins the other half: when the upstream
// answers with a finish_reason but no token count, the row used to come back with
// an empty verdict, which printed as a blank 判据 column and read like the tool
// had silently given up.
func TestProbeModelNeverLeavesTheVerdictBlank(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"1\"},\"finish_reason\":\"length\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	res := probeModel(srv.Client(), srv.URL, "k", "cn:m", []int{40000}, 1, true, 0, nil)
	if strings.TrimSpace(res.Verdict) == "" {
		t.Fatal("the verdict is blank; an operator reading the table learns nothing")
	}
	if res.Measured != nil {
		t.Errorf("measured = %d, want none when no usage arrived", *res.Measured)
	}
	if len(res.Evidence) == 0 {
		t.Error("no evidence recorded for a rung that answered")
	}
	if verdict, measured := classify(res); verdict != "inconclusive" || measured != nil {
		t.Errorf("classify = (%q, %v), want (inconclusive, none)", verdict, measured)
	}
}

func TestProbeOnceReportsAnHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream exploded", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := probeOnce(srv.Client(), srv.URL+"/v1", "k", "cn:m", 100, "count")
	if err == nil {
		t.Fatal("want an error for HTTP 500")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error = %q, want it to name the status", err)
	}
}

// ---------------------------------------------------------------------------
// The model catalogue
// ---------------------------------------------------------------------------

func TestFetchModelsToleratesBothShapesAndStringEntries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("x-api-key"); got != "k" {
			t.Errorf("x-api-key = %q", got)
		}
		fmt.Fprint(w, `{"data":[
			"cn:bare-string",
			{"id":"cn:object","max_output_tokens":131072,"context_length":200000},
			{"name":"cn:by-name","max_completion_tokens":32000},
			{"id":"cn:top-provider","top_provider":{"max_completion_tokens":8000}},
			{"no_id":true}
		]}`)
	}))
	defer srv.Close()

	got, err := fetchModels(srv.Client(), srv.URL+"/v1/", "k")
	if err != nil {
		t.Fatalf("fetchModels: %v", err)
	}
	want := []struct {
		id      string
		claimed *int
	}{
		{"cn:bare-string", nil},
		{"cn:object", intPtr(131072)},
		{"cn:by-name", intPtr(32000)},
		{"cn:top-provider", intPtr(8000)},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d models, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].ID != w.id {
			t.Errorf("[%d] id = %q, want %q", i, got[i].ID, w.id)
		}
		switch {
		case w.claimed == nil && got[i].ClaimedOutput != nil:
			t.Errorf("[%d] claimed = %d, want none", i, *got[i].ClaimedOutput)
		case w.claimed != nil && got[i].ClaimedOutput == nil:
			t.Errorf("[%d] claimed = none, want %d", i, *w.claimed)
		case w.claimed != nil && *got[i].ClaimedOutput != *w.claimed:
			t.Errorf("[%d] claimed = %d, want %d", i, *got[i].ClaimedOutput, *w.claimed)
		}
	}
	if got[1].ClaimedCtx == nil || *got[1].ClaimedCtx != 200000 {
		t.Errorf("context_length was not read: %+v", got[1])
	}
}

// TestFetchModelsReadsTheExtraBlockThisGatewayPublishes pins the shape this
// repository's own gateway answers with: a module's per-model numbers live under
// "extra" (internal/gateway/openai.go modelEntry.Extra), so a parser that reads
// only the top level reports "声称 -" for every model of a catalogue that does
// publish a ceiling — which is exactly what the live dry-run did.
func TestFetchModelsReadsTheExtraBlockThisGatewayPublishes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"object":"list","data":[
			{"id":"loomy/deepseek-v4-flash-0731","object":"model","owned_by":"loomy",
			 "extra":{"context_length":1048576,"max_output_tokens":384000,"display_name":"DeepSeek V4 Flash 0731 · x3.0"}},
			{"id":"zcode/glm-5.3","object":"model","owned_by":"zcode","extra":{"display_name":"GLM-5.3"}},
			{"id":"Auto/GLM-5.3","object":"model","owned_by":"auto","extra":{"target":"glm-5.3"}},
			{"id":"flat/top-level","max_output_tokens":65536}
		]}`)
	}))
	defer srv.Close()

	got, err := fetchModels(srv.Client(), srv.URL+"/v1", "k")
	if err != nil {
		t.Fatalf("fetchModels: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d models, want 3: %+v", len(got), got)
	}
	for _, m := range got {
		if strings.HasPrefix(m.ID, "Auto/") {
			t.Errorf("fetchModels kept the gateway virtual model %q", m.ID)
		}
	}
	if got[0].ClaimedOutput == nil || *got[0].ClaimedOutput != 384000 {
		t.Errorf("extra.max_output_tokens was not read: %+v", got[0])
	}
	if got[0].ClaimedCtx == nil || *got[0].ClaimedCtx != 1048576 {
		t.Errorf("extra.context_length was not read: %+v", got[0])
	}
	// A module that publishes only a display name must not grow a claim.
	if got[1].ClaimedOutput != nil || got[1].ClaimedCtx != nil {
		t.Errorf("a model with no numbers in extra grew a claim: %+v", got[1])
	}
	// The flat spelling still works: this gateway is not the only upstream.
	if got[2].ClaimedOutput == nil || *got[2].ClaimedOutput != 65536 {
		t.Errorf("the top-level spelling regressed: %+v", got[2])
	}
}

func TestFetchModelsReadsABareArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":"cn:one"}]`)
	}))
	defer srv.Close()

	got, err := fetchModels(srv.Client(), srv.URL, "k")
	if err != nil {
		t.Fatalf("fetchModels: %v", err)
	}
	if len(got) != 1 || got[0].ID != "cn:one" {
		t.Errorf("got %+v", got)
	}
}

func TestFetchModelsReportsANonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := fetchModels(srv.Client(), srv.URL, "k"); err == nil {
		t.Fatal("want an error for HTTP 401")
	}
}

// ---------------------------------------------------------------------------
// The ladder
// ---------------------------------------------------------------------------

// ladderServer answers by rung: it reads max_tokens from the request and calls
// reply, so a test can describe the upstream's clamping behaviour as a table.
func ladderServer(t *testing.T, reply func(rung int, attempt int) (out int, finish string)) *httptest.Server {
	t.Helper()
	var (
		mu       sync.Mutex
		attempts = map[int]int{}
	)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.Unmarshal(body, &req)

		mu.Lock()
		attempts[req.MaxTokens]++
		n := attempts[req.MaxTokens]
		mu.Unlock()

		out, finish := reply(req.MaxTokens, n)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":%q}],\"usage\":{\"completion_tokens\":%d}}\n\n", finish, out)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func TestProbeModelClimbsUntilTheUpstreamClamps(t *testing.T) {
	// 40000 comes back full, 100000 comes back short: the real ceiling is
	// somewhere at or below 48000, and the verdict has to say so rather than
	// pretend 48000 was requested.
	srv := ladderServer(t, func(rung, _ int) (int, string) {
		if rung >= 100000 {
			return 48000, "length"
		}
		return rung, "length"
	})
	defer srv.Close()

	res := probeModel(srv.Client(), srv.URL, "k", "cn:m", []int{40000, 100000}, 3, true, 0, nil)
	if res.Measured == nil || *res.Measured != 48000 {
		t.Fatalf("measured = %v, want 48000 (verdict %q)", res.Measured, res.Verdict)
	}
	// The bare wording is the normal case: the clamp came in above the last full
	// rung, so there is no contradiction to explain.  The bracketed form is only
	// for an upstream whose own numbers disagree with each other.
	if res.Verdict != "钳制" {
		t.Errorf("verdict = %q, want the bare clamp wording", res.Verdict)
	}
	verdict, measured := classify(res)
	if verdict != "clamped" || measured == nil || *measured != 48000 {
		t.Errorf("classify = (%q, %v), want (clamped, 48000)", verdict, measured)
	}
	if len(res.Evidence) == 0 {
		t.Error("evidence should record the rungs that were tried")
	}
}

func TestProbeModelBracketsAClampBelowTheLastFullRung(t *testing.T) {
	// A noisy upstream: a bigger ask came back with fewer tokens than a smaller
	// one already produced.  The verdict has to name both numbers, because
	// neither one alone tells the operator what to write into the config.
	srv := ladderServer(t, func(rung, _ int) (int, string) {
		if rung >= 100000 {
			return 30000, "length"
		}
		return rung, "length"
	})
	defer srv.Close()

	res := probeModel(srv.Client(), srv.URL, "k", "cn:m", []int{40000, 100000}, 3, true, 0, nil)
	if !strings.HasPrefix(res.Verdict, "≈30000（钳制；上一档满额 40000") {
		t.Errorf("verdict = %q, want it to bracket the clamp with the previous full rung", res.Verdict)
	}
	verdict, measured := classify(res)
	if verdict != "clamped" || measured == nil || *measured != 30000 {
		t.Errorf("classify = (%q, %v), want (clamped, 30000)", verdict, measured)
	}
}

func TestProbeModelReportsALowerBoundWhenTheTopRungIsFull(t *testing.T) {
	srv := ladderServer(t, func(rung, _ int) (int, string) { return rung, "length" })
	defer srv.Close()

	res := probeModel(srv.Client(), srv.URL, "k", "cn:m", []int{1000, 2000}, 3, true, 0, nil)
	verdict, measured := classify(res)
	if verdict != "at_least" || measured == nil || *measured != 2000 {
		t.Fatalf("classify = (%q, %v), want (at_least, 2000); verdict was %q", verdict, measured, res.Verdict)
	}
}

func TestProbeModelDoesNotCountReasoningTokensAsOutput(t *testing.T) {
	// The live shape that motivated this: max_tokens=2000, and the upstream
	// answered completion_tokens=3219 with reasoning_tokens=1214.  Only 2005
	// tokens of actual answer were emitted, but the total is 60% larger than
	// anything the model was allowed to emit — publishing 3219 as the measured
	// ceiling, which is what --panel-out does, would be a lie.
	for _, tc := range []struct {
		name     string
		tier     int
		verdict  string
		measured int
	}{
		{"clamped below the rung", 40000, "clamped", 2005},
		{"full at the rung", 2000, "at_least", 2005},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := sseServer(t, "length", 3219, 1214)
			defer srv.Close()

			res := probeModel(srv.Client(), srv.URL+"/v1", "k", "cn:m", []int{tc.tier}, 1, true, 0, nil)
			verdict, measured := classify(res)
			if measured == nil {
				t.Fatalf("measured = nil, want %d; verdict was %q", tc.measured, res.Verdict)
			}
			if *measured == 3219 {
				t.Errorf("measured = 3219, the reasoning tokens were counted as output")
			}
			if verdict != tc.verdict || *measured != tc.measured {
				t.Errorf("classify = (%q, %d), want (%q, %d); verdict was %q",
					verdict, *measured, tc.verdict, tc.measured, res.Verdict)
			}
		})
	}
}

func TestProbeModelRetriesWithFirmerWordingBeforeGivingUp(t *testing.T) {
	// Every rung ends with the model stopping on its own.  The retry must use the
	// hard prompt, and the final answer must be a lower bound — not "no idea".
	var prompts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		if len(req.Messages) > 0 {
			prompts = append(prompts, req.Messages[0].Content)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":3000}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	res := probeModel(srv.Client(), srv.URL, "k", "cn:m", []int{40000}, 2, true, 0, nil)
	verdict, measured := classify(res)
	if verdict != "at_least" || measured == nil || *measured != 3000 {
		t.Fatalf("classify = (%q, %v), want (at_least, 3000); verdict was %q", verdict, measured, res.Verdict)
	}
	if len(prompts) != 2 {
		t.Fatalf("made %d attempts, want 2", len(prompts))
	}
	if prompts[0] == prompts[1] {
		t.Error("the retry re-sent the same prompt; a model that ignored it once will ignore it again")
	}
	if !strings.Contains(prompts[1], "Count from 1 to") {
		t.Errorf("the retry prompt was %q, want the firmer English wording", prompts[1])
	}
}

func TestProbeModelReportsTotalFailureRatherThanAGuess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()

	res := probeModel(srv.Client(), srv.URL, "k", "cn:m", []int{40000}, 3, true, 0, nil)
	if res.Measured != nil {
		t.Errorf("measured = %d, want none when nothing answered", *res.Measured)
	}
	verdict, measured := classify(res)
	if verdict != "inconclusive" || measured != nil {
		t.Errorf("classify = (%q, %v), want (inconclusive, none)", verdict, measured)
	}
}

func TestProbeModelStopsWhenTheBudgetIsExhausted(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}],\"usage\":{\"completion_tokens\":1}}\n\n")
	}))
	defer srv.Close()

	// A budget already in the past: the ladder must return immediately with the
	// "did not finish" verdict instead of spending the whole tier list.
	res := probeModel(srv.Client(), srv.URL, "k", "cn:m", []int{40000, 100000}, 3, true, time.Nanosecond, nil)
	if !strings.HasPrefix(res.Verdict, "未测完") {
		t.Errorf("verdict = %q, want the budget-exhausted wording", res.Verdict)
	}
	if calls != 0 {
		t.Errorf("made %d upstream calls under an exhausted budget, want 0", calls)
	}
}

// TestProbeModelWithZeroRetriesStillSendsOneRequest pins how the -retries flag
// is read: it counts RETRIES after a finish=stop, so zero must still ask once.
// The loop used to run `attempt <= retries`, which made `-retries 0` send
// nothing at all and then report the model as unavailable — with an empty
// evidence list, so nothing in the output hinted that no request was made.
func TestProbeModelWithZeroRetriesStillSendsOneRequest(t *testing.T) {
	var calls int
	srv := ladderServer(t, func(rung, _ int) (int, string) {
		calls++
		return rung, "length"
	})
	defer srv.Close()

	res := probeModel(srv.Client(), srv.URL, "k", "cn:m", []int{40000}, 0, true, 0, nil)
	if calls == 0 {
		t.Fatal("with -retries 0 the probe sent no request at all")
	}
	if res.Measured == nil || *res.Measured != 40000 {
		t.Errorf("measured = %v, want 40000: %+v", res.Measured, res)
	}
	if len(res.Evidence) == 0 {
		t.Error("a run that did send a request left no evidence behind")
	}
}

func TestProbeModelCarriesTheClaimedCeilingIntoTheNote(t *testing.T) {
	srv := ladderServer(t, func(rung, _ int) (int, string) { return 3000, "stop" })
	defer srv.Close()

	claimed := 131072
	res := probeModel(srv.Client(), srv.URL, "k", "cn:m", []int{40000}, 1, true, 0, &claimed)
	// The reference read the claimed value before it had been recorded, so its
	// note always said "声称 ?".  This port passes it in, and the note should
	// name the number the operator can compare against.
	if !strings.Contains(res.Verdict, "131072") {
		t.Errorf("verdict = %q, want it to name the claimed ceiling", res.Verdict)
	}
}

// ---------------------------------------------------------------------------
// Resumability
// ---------------------------------------------------------------------------

func TestReadJSONLSkipsATornLastLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probe-max-tokens.jsonl")
	body := `{"model":"cn:a","verdict":"钳制","measured":10}` + "\n" +
		`{"model":"cn:b","verdict":"≥20"}` + "\n" +
		`{"model":"cn:c","verdict":"half-writ` // an interrupted run's last line
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readJSONL(path)
	if err != nil {
		t.Fatalf("readJSONL: %v", err)
	}
	// Two usable records: a torn line must not make the whole resume fail, or
	// the operator loses every measurement from the interrupted run.
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2: %+v", len(got), got)
	}
	if got[0].Model != "cn:a" || got[1].Model != "cn:b" {
		t.Errorf("models = %q, %q", got[0].Model, got[1].Model)
	}
}

func TestReadJSONLOnAMissingFileIsNotFatal(t *testing.T) {
	if _, err := readJSONL(filepath.Join(t.TempDir(), "absent.jsonl")); err == nil {
		t.Error("want an error for a missing file so the caller can tell the difference")
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func TestClipCountsRunesNotBytes(t *testing.T) {
	// Chinese error messages are the common case here; cutting mid-character
	// produces a replacement glyph in a table cell.
	got := clip("请求失败：上游返回了很长的错误信息", 6)
	if got != "请求失败：上" {
		t.Errorf("clip = %q, want the first six runes", got)
	}
	if clip("short", 40) != "short" {
		t.Error("clip should leave a short string alone")
	}
}

func TestClaimedAndMeasuredTextUseADashForUnknown(t *testing.T) {
	if claimedText(nil) != "-" || measuredText(nil) != "-" {
		t.Error("an unknown value should render as a dash, not as 0")
	}
	if claimedText(intPtr(0)) != "0" {
		t.Error("a real zero must render as 0, not as a dash")
	}
	if tokenText(5, false) != "-" {
		t.Error("a missing usage block should render as a dash")
	}
	if tokenText(5, true) != "5" {
		t.Error("a reported value should render as itself")
	}
}

func TestDirOfHandlesBothSeparators(t *testing.T) {
	cases := map[string]string{
		`data/output_probes.json`: "data",
		`a\b\c.json`:              `a\b`,
		`plain.json`:              "",
	}
	for in, want := range cases {
		if got := dirOf(in); got != want {
			t.Errorf("dirOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNamespaceOfSplitsOnTheClientPrefix(t *testing.T) {
	cases := map[string]string{
		"zcode/glm-5.2":     "zcode/",
		"workbuddy/glm-5.2": "workbuddy/",
		"cn:glm-5.2":        "(无客户端前缀)",
		"bare":              "(无客户端前缀)",
	}
	for in, want := range cases {
		if got := namespaceOf(in); got != want {
			t.Errorf("namespaceOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSelectorHelpListsTheClientsThisGatewayServes(t *testing.T) {
	all := []modelTarget{
		{ID: "zcode/glm-5.2"},
		{ID: "zcode/glm-4.7"},
		{ID: "workbuddy/glm-5.2"},
		{ID: "kimi/kimi-k2"},
		{ID: "bare-model"},
	}
	got := selectorHelp(all)
	// The whole point of the message is that a bare run tells the operator
	// which prefix to type; a generic usage line would leave them guessing.
	if !strings.Contains(got, "共 5 个模型可测") {
		t.Errorf("selectorHelp should state the catalogue size, got %q", got)
	}
	for _, ns := range []string{"zcode/(2)", "workbuddy/(1)", "kimi/(1)", "(无客户端前缀)(1)"} {
		if !strings.Contains(got, ns) {
			t.Errorf("selectorHelp should list %q, got %q", ns, got)
		}
	}
	// The reference's default namespace must not appear: this gateway has no
	// `cn:` models, which is exactly why the old default matched nothing.
	if strings.Contains(got, "--prefix cn:") {
		t.Error("selectorHelp still suggests the reference's cn: default")
	}
	if !strings.Contains(got, "--models") || !strings.Contains(got, "--limit") {
		t.Error("selectorHelp should offer every way to narrow the set")
	}
}

func TestNoMatchHelpNamesTheSelectorThatFilteredEverythingOut(t *testing.T) {
	all := []modelTarget{{ID: "zcode/glm-5.2"}, {ID: "kimi/kimi-k2"}}

	byPrefix := noMatchHelp(all, "", []string{"cn:"})
	if !strings.Contains(byPrefix, "--prefix cn:") {
		t.Errorf("a prefix miss should name the prefix, got %q", byPrefix)
	}
	if !strings.Contains(byPrefix, "共 2 个可测") || !strings.Contains(byPrefix, "zcode/(1)") {
		t.Errorf("a prefix miss should still show what is available, got %q", byPrefix)
	}

	byModels := noMatchHelp(all, "gpt-9", nil)
	if !strings.Contains(byModels, `--models "gpt-9"`) {
		t.Errorf("a keyword miss should name the keyword, got %q", byModels)
	}
}
