package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// catalogue helpers
// ---------------------------------------------------------------------------

func geminiIDs(list []core.Model) []string {
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.ID)
	}
	return out
}

func geminiEntry(list []core.Model, id string) (core.Model, bool) {
	for _, m := range list {
		if m.ID == id {
			return m, true
		}
	}
	return core.Model{}, false
}

func extraStrings(extra map[string]any, key string) []string {
	switch v := extra[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, _ := item.(string)
			out = append(out, s)
		}
		return out
	default:
		return nil
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

// ---------------------------------------------------------------------------
// fetchAvailableModels
// ---------------------------------------------------------------------------

func TestRefreshModelsReadsTheVendorCatalogue(t *testing.T) {
	var (
		mu      sync.Mutex
		gotPath string
		gotAuth string
		gotBody map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case fetchAvailableModelsPath:
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &gotBody)
			mu.Lock()
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{
			  "models": {
			    "gemini-pro": {"displayName":"Gemini 3.1 Pro","model":"gemini-3.1-pro","maxTokens":1000000,"maxOutputTokens":64000,"supportsImages":true,"supportsThinking":true},
			    "chat_hidden": {"displayName":"Hidden Chat","model":"chat_hidden-model"},
			    "tab_hidden": {"displayName":"Hidden Tab","model":"tab_hidden-model"},
			    "retired": {"displayName":"Retired","model":"gemini-9-retired","disabled":true},
			    "gemini-3.7-flash-low": {"displayName":"Gemini 3.7 Flash Low","model":"gemini-3.7-flash-low","maxTokens":1000000},
			    "gemini-3.7-flash-medium": {"displayName":"Gemini 3.7 Flash Medium","model":"gemini-3.7-flash-medium","maxTokens":1000000},
			    "gemini-3.7-flash-high": {"displayName":"Gemini 3.7 Flash High","model":"gemini-3.7-flash-high","maxTokens":1000000}
			  },
			  "agentModelSorts": [{"groups":[{"modelIds":[
			    "gemini-3.7-flash-medium","gemini-pro","gemini-3.7-flash-high","gemini-3.7-flash-low"
			  ]}]}]
			}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, `{"project":"proj","accounts":[{"access_token":"tok","expiry":"`+timeNowPlusHour()+`"}]}`)
	restore := overrideEndpoints(srv.URL)
	defer restore()

	list, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}

	mu.Lock()
	path, auth, body := gotPath, gotAuth, gotBody
	mu.Unlock()
	if path != fetchAvailableModelsPath {
		t.Fatalf("upstream path = %q, want %q", path, fetchAvailableModelsPath)
	}
	if auth != "Bearer tok" {
		t.Errorf("Authorization = %q, want Bearer tok", auth)
	}
	if body["project"] != "proj" {
		t.Errorf("request project = %v, want proj", body["project"])
	}

	want := []string{"gemini-3.7-flash", "gemini-3.1-pro"}
	if got := geminiIDs(list); !equalStrings(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for _, hidden := range []string{"chat_hidden-model", "tab_hidden-model", "gemini-9-retired"} {
		if _, ok := geminiEntry(list, hidden); ok {
			t.Errorf("%s must be filtered out", hidden)
		}
	}

	family, ok := geminiEntry(list, "gemini-3.7-flash")
	if !ok {
		t.Fatal("the effort family did not fold into its base id")
	}
	if got := extraStrings(family.Extra, "supported_efforts"); !equalStrings(got, []string{"low", "medium", "high"}) {
		t.Errorf("supported_efforts = %v, want [low medium high]", got)
	}
	if family.Extra["default_effort"] != "high" {
		t.Errorf("default_effort = %v, want high (no bare variant exists)", family.Extra["default_effort"])
	}
	if family.Extra["source"] != modelsSourceUpstream {
		t.Errorf("source = %v, want %q", family.Extra["source"], modelsSourceUpstream)
	}

	pro, ok := geminiEntry(list, "gemini-3.1-pro")
	if !ok {
		t.Fatal("gemini-3.1-pro is missing")
	}
	if pro.Extra["display_name"] != "Gemini 3.1 Pro" {
		t.Errorf("display_name = %v", pro.Extra["display_name"])
	}
	if pro.Extra["context_length"] != 1000000 {
		t.Errorf("context_length = %v, want 1000000", pro.Extra["context_length"])
	}
	if pro.Extra["max_output_tokens"] != 64000 {
		t.Errorf("max_output_tokens = %v, want 64000", pro.Extra["max_output_tokens"])
	}
	if pro.Extra["supports_image"] != true {
		t.Errorf("supports_image = %v, want true", pro.Extra["supports_image"])
	}
}

// ---------------------------------------------------------------------------
// retrieveUserQuota fallback
// ---------------------------------------------------------------------------

func TestRefreshModelsFallsBackToQuotaBuckets(t *testing.T) {
	var quotaHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case fetchAvailableModelsPath:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
		case retrieveUserQuotaPath:
			atomic.AddInt32(&quotaHits, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"buckets":[
			  {"bucketId":"gemini-3.1-pro"},
			  {"modelId":"claude-sonnet-4-5"},
			  {"bucketId":"gemini-3.1-pro"}
			]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, `{"project":"proj","accounts":[{"access_token":"tok"}]}`)
	restore := overrideEndpoints(srv.URL)
	defer restore()

	list, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if atomic.LoadInt32(&quotaHits) != 1 {
		t.Fatalf("retrieveUserQuota hits = %d, want 1", quotaHits)
	}
	want := []string{"gemini-3.1-pro", "claude-sonnet-4-5"}
	if got := geminiIDs(list); !equalStrings(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for _, m := range list {
		if m.Extra["discovery"] != modelsDiscoveryQuota {
			t.Errorf("%s discovery = %v, want %q", m.ID, m.Extra["discovery"], modelsDiscoveryQuota)
		}
		if m.Extra["source"] != modelsSourceUpstream {
			t.Errorf("%s source = %v, want %q", m.ID, m.Extra["source"], modelsSourceUpstream)
		}
	}
}

// ---------------------------------------------------------------------------
// cache behaviour
// ---------------------------------------------------------------------------

func TestRefreshModelsKeepsTheLastGoodListOnFailure(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case fetchAvailableModelsPath:
			if atomic.AddInt32(&calls, 1) == 1 {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"models":{"gemini-3.1-pro":{"displayName":"Gemini 3.1 Pro","model":"gemini-3.1-pro"}}}`)
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
		case retrieveUserQuotaPath:
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"message":"nope"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, `{"project":"proj","accounts":[{"access_token":"tok"}]}`)
	restore := overrideEndpoints(srv.URL)
	defer restore()

	first, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("first RefreshModels: %v", err)
	}
	if len(first) != 1 || first[0].ID != "gemini-3.1-pro" {
		t.Fatalf("first list = %v", geminiIDs(first))
	}

	second, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("a failed refresh must report an error")
	}
	if got := geminiIDs(second); !equalStrings(got, []string{"gemini-3.1-pro"}) {
		t.Fatalf("failed refresh list = %v, want the last good list", got)
	}

	served, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models after a failed refresh: %v", err)
	}
	if got := geminiIDs(served); !equalStrings(got, []string{"gemini-3.1-pro"}) {
		t.Fatalf("Models = %v, want the last good list", got)
	}
}

func TestModelsWithoutAnAccountServesTheMarkedFallback(t *testing.T) {
	c := newTestClient(t, `{}`)
	list, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("the fallback catalogue must not be empty")
	}
	if _, ok := geminiEntry(list, upstreamFlash); !ok {
		t.Fatalf("fallback ids = %v, want %s", geminiIDs(list), upstreamFlash)
	}
	for _, m := range list {
		if m.Extra["source"] != modelsSourceFallback {
			t.Errorf("%s source = %v, want %q", m.ID, m.Extra["source"], modelsSourceFallback)
		}
		if m.Extra["fallback"] != true {
			t.Errorf("%s fallback = %v, want true", m.ID, m.Extra["fallback"])
		}
	}
}

// ---------------------------------------------------------------------------
// routing
// ---------------------------------------------------------------------------

// recordingUpstream serves the catalogue plus one SSE stream per chat call and
// records every chat request body it sees.
func recordingUpstream(t *testing.T, catalogue string) (*httptest.Server, *[]string) {
	t.Helper()
	var (
		mu     sync.Mutex
		bodies []string
	)
	// streamPath carries the SSE query string; the server sees only the path.
	inferencePath, _, _ := strings.Cut(streamPath, "?")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case fetchAvailableModelsPath:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, catalogue)
		case inferencePath:
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, string(raw))
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"response":{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}}`+"\n\n")
			_, _ = io.WriteString(w, `data: [DONE]`+"\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	return srv, &bodies
}

func lastChatBody(t *testing.T, bodies *[]string) string {
	t.Helper()
	if bodies == nil || len(*bodies) == 0 {
		t.Fatal("the upstream saw no chat request")
	}
	return (*bodies)[len(*bodies)-1]
}

func TestChatAcceptsADynamicModelAndForwardsItVerbatim(t *testing.T) {
	srv, bodies := recordingUpstream(t, `{"models":{"claude-sonnet-4-5":{"displayName":"Claude Sonnet 4.5","model":"claude-sonnet-4-5"}}}`)
	defer srv.Close()

	c := newTestClient(t, `{"project":"proj","accounts":[{"access_token":"tok"}]}`)
	restore := overrideEndpoints(srv.URL)
	defer restore()

	if _, err := c.RefreshModels(context.Background()); err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	collect(t, stream)

	if body := lastChatBody(t, bodies); !strings.Contains(body, `"model":"claude-sonnet-4-5"`) {
		t.Errorf("upstream body did not forward the exact id: %s", body)
	}
}

func TestChatRoutesAnEffortFamilyByReasoningEffort(t *testing.T) {
	srv, bodies := recordingUpstream(t, `{"models":{
	  "gemini-3.7-flash-low":{"displayName":"Gemini 3.7 Flash Low","model":"gemini-3.7-flash-low"},
	  "gemini-3.7-flash-medium":{"displayName":"Gemini 3.7 Flash Medium","model":"gemini-3.7-flash-medium"},
	  "gemini-3.7-flash-high":{"displayName":"Gemini 3.7 Flash High","model":"gemini-3.7-flash-high"}
	}}`)
	defer srv.Close()

	c := newTestClient(t, `{"project":"proj","accounts":[{"access_token":"tok"}]}`)
	restore := overrideEndpoints(srv.URL)
	defer restore()

	list, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if got := geminiIDs(list); !equalStrings(got, []string{"gemini-3.7-flash"}) {
		t.Fatalf("ids = %v, want the folded family only", got)
	}

	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "gemini-3.7-flash",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Options:  map[string]any{"reasoning_effort": "high"},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	collect(t, stream)

	body := lastChatBody(t, bodies)
	if !strings.Contains(body, `"model":"gemini-3.7-flash-high"`) {
		t.Errorf("high effort was not mapped to the upstream variant: %s", body)
	}
	if !strings.Contains(body, `"thinkingBudget":10000`) {
		t.Errorf("the high-effort budget is missing: %s", body)
	}
}

func TestChatRoutesAnEffortFamilyDefaultToHigh(t *testing.T) {
	srv, bodies := recordingUpstream(t, `{"models":{
	  "gemini-3.7-flash-low":{"model":"gemini-3.7-flash-low"},
	  "gemini-3.7-flash-high":{"model":"gemini-3.7-flash-high"}
	}}`)
	defer srv.Close()

	c := newTestClient(t, `{"project":"proj","accounts":[{"access_token":"tok"}]}`)
	restore := overrideEndpoints(srv.URL)
	defer restore()
	if _, err := c.RefreshModels(context.Background()); err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}

	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "gemini-3.7-flash",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	collect(t, stream)
	if body := lastChatBody(t, bodies); !strings.Contains(body, `"model":"gemini-3.7-flash-high"`) {
		t.Errorf("the no-effort default did not pick high: %s", body)
	}
}

func TestTieredModelStaysASeparateAdaptiveID(t *testing.T) {
	srv, bodies := recordingUpstream(t, `{"models":{
	  "gemini-3.7-flash-low":{"model":"gemini-3.7-flash-low"},
	  "gemini-3.7-flash-high":{"model":"gemini-3.7-flash-high"},
	  "gemini-3.7-flash-tiered":{"displayName":"Gemini 3.7 Flash Tiered","model":"gemini-3.7-flash-tiered"}
	}}`)
	defer srv.Close()

	c := newTestClient(t, `{"project":"proj","accounts":[{"access_token":"tok"}]}`)
	restore := overrideEndpoints(srv.URL)
	defer restore()
	list, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	want := []string{"gemini-3.7-flash", "gemini-3.7-flash-tiered"}
	if got := geminiIDs(list); !equalStrings(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	tiered, ok := geminiEntry(list, "gemini-3.7-flash-tiered")
	if !ok {
		t.Fatal("the tiered id must stay visible")
	}
	if got := extraStrings(tiered.Extra, "supported_efforts"); !equalStrings(got, []string{"low", "high"}) {
		t.Errorf("tiered supported_efforts = %v, want the sibling ladder", got)
	}

	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "gemini-3.7-flash-tiered",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Options:  map[string]any{"reasoning_effort": "low"},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	collect(t, stream)
	body := lastChatBody(t, bodies)
	if !strings.Contains(body, `"model":"gemini-3.7-flash-tiered"`) {
		t.Errorf("the tiered id was not forwarded verbatim: %s", body)
	}
	if strings.Contains(body, "thinkingBudget") {
		t.Errorf("the adaptive tier must omit thinkingBudget: %s", body)
	}
}

func TestALoneVariantKeepsItsOwnThinkingBudget(t *testing.T) {
	// A family the vendor serves as a single variant does not fold, so the id
	// stays exactly as the vendor spells it -- budget included.
	srv, bodies := recordingUpstream(t, `{"models":{
	  "gemini-3.7-flash-high":{"displayName":"Gemini 3.7 Flash High","model":"gemini-3.7-flash-high"}
	}}`)
	defer srv.Close()

	c := newTestClient(t, `{"project":"proj","accounts":[{"access_token":"tok"}]}`)
	restore := overrideEndpoints(srv.URL)
	defer restore()
	list, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if got := geminiIDs(list); !equalStrings(got, []string{"gemini-3.7-flash-high"}) {
		t.Fatalf("ids = %v, want the lone variant only", got)
	}

	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "gemini-3.7-flash-high",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	collect(t, stream)
	body := lastChatBody(t, bodies)
	if !strings.Contains(body, `"model":"gemini-3.7-flash-high"`) {
		t.Errorf("the lone variant was not forwarded verbatim: %s", body)
	}
	if !strings.Contains(body, `"thinkingBudget":10000`) {
		t.Errorf("the id's own level did not carry a budget: %s", body)
	}
}

func TestChatStillRejectsAnUnknownModelAfterARefresh(t *testing.T) {
	srv, _ := recordingUpstream(t, `{"models":{"gemini-3.1-pro":{"model":"gemini-3.1-pro"}}}`)
	defer srv.Close()

	c := newTestClient(t, `{"project":"proj","accounts":[{"access_token":"tok"}]}`)
	restore := overrideEndpoints(srv.URL)
	defer restore()
	if _, err := c.RefreshModels(context.Background()); err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if _, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "gemini-9.9-fake",
		Messages: []core.Message{{Role: "user", Content: "x"}},
	}); err == nil {
		t.Fatal("an unknown model was accepted after a dynamic refresh")
	}
}
