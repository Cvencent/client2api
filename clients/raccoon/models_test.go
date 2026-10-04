package raccoon

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

func TestFormatMultiplier(t *testing.T) {
	cases := map[float64]string{
		1:                   "1",
		0.75:                "0.75",
		1.5:                 "1.5",
		0.1:                 "0.1",
		0.30000000000000004: "0.3",
		0:                   "0",
		0.123456:            "0.1235",
	}
	for in, want := range cases {
		if got := formatMultiplier(in); got != want {
			t.Errorf("formatMultiplier(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestDisplayNameRules(t *testing.T) {
	nan := math.NaN()
	inf := math.Inf(1)
	cases := []struct {
		name        string
		id          string
		description string
		effective   *float64
		base        *float64
		want        string
	}{
		{
			name: "free model shows 免费, never x0",
			id:   "sn-sensenova-6-8-flash", description: "SenseNova-6.8-Flash",
			effective: f64(0), base: f64(0.5),
			want: "SenseNova-6.8-Flash · 免费",
		},
		{
			name: "discounted model shows the base->effective arrow",
			id:   "sn-glm-5-3-flash", description: "GLM-5-3-Flash",
			effective: f64(0.1), base: f64(0.2),
			want: "GLM-5-3-Flash · x0.2→x0.1",
		},
		{
			name: "1x IS shown (a real user-reported defect)",
			id:   "sn-kimi-k3", description: "Kimi-K3",
			effective: f64(1), base: f64(1),
			want: "Kimi-K3 · x1",
		},
		{
			name: "undiscounted non-unit multiplier",
			id:   "sn-glm-5-3", description: "GLM-5-3",
			effective: f64(0.75), base: f64(0.75),
			want: "GLM-5-3 · x0.75",
		},
		{
			name: "no base multiplier at all",
			id:   "sn-deepseek-v4-1-flash", description: "DeepSeek-V4.1-Flash",
			effective: f64(0.25),
			want:      "DeepSeek-V4.1-Flash · x0.25",
		},
		{
			name: "base lower than effective is not an arrow",
			id:   "x", description: "X",
			effective: f64(2), base: f64(1),
			want: "X · x2",
		},
		{
			name: "absent multiplier appends NOTHING (no dangling separator)",
			id:   "sn-kimi-k3", description: "Kimi-K3",
			want: "Kimi-K3",
		},
		{
			name: "NaN multiplier appends nothing",
			id:   "x", description: "X", effective: &nan,
			want: "X",
		},
		{
			name: "infinite multiplier appends nothing",
			id:   "x", description: "X", effective: &inf,
			want: "X",
		},
		{
			name: "negative multiplier appends nothing",
			id:   "x", description: "X", effective: f64(-1),
			want: "X",
		},
		{
			name: "empty description falls back to the id",
			id:   "sn-kimi-k3",
			want: "sn-kimi-k3",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := displayName(c.id, c.description, c.effective, c.base)
			if got != c.want {
				t.Fatalf("displayName = %q, want %q", got, c.want)
			}
			if strings.HasSuffix(got, " · ") || strings.HasSuffix(got, "· ") {
				t.Fatalf("displayName %q ends with a dangling separator", got)
			}
		})
	}
}

func TestStripMultiplierAndMapModel(t *testing.T) {
	if got := stripMultiplier("Kimi-K3 · x1"); got != "Kimi-K3" {
		t.Fatalf("stripMultiplier = %q", got)
	}
	if got := stripMultiplier("Kimi-K3"); got != "Kimi-K3" {
		t.Fatalf("stripMultiplier = %q", got)
	}
	if got := mapModel("raccoon/sn-kimi-k3"); got != "sn-kimi-k3" {
		t.Fatalf("mapModel = %q, want the routing prefix stripped", got)
	}
	if got := mapModel("Kimi-K3 · x1"); got != "Kimi-K3" {
		t.Fatalf("mapModel = %q, want the price suffix stripped", got)
	}
	if got := mapModel("  sn-kimi-k3  "); got != "sn-kimi-k3" {
		t.Fatalf("mapModel = %q", got)
	}
}

const catalogFixture = `{
  "code": 0,
  "message": "",
  "details": "",
  "data": {
    "categories": [
      {
        "type": "embedding",
        "models": [{"name": "embed-1", "visible": true, "description": "Embed"}]
      },
      {
        "type": "chat",
        "models": [
          {
            "name": "sn-kimi-k3",
            "visible": true,
            "description": "Kimi-K3",
            "billing_effective_multiplier": 1,
            "billing_multiplier": 1,
            "billing_status": "normal",
            "params": {"context_window": 1000000, "max_tokens": 100000},
            "tags": ["vision", "Image-Understanding"]
          },
          {
            "name": "sn-sensenova-6-8-flash",
            "visible": true,
            "description": "SenseNova-6.8-Flash",
            "billing_effective_multiplier": "0",
            "billing_multiplier": "0.5",
            "params": {"context_window": 256000, "max_tokens": 63999}
          },
          {
            "name": "sn-glm-5-3-flash",
            "description": "GLM-5-3-Flash",
            "billing_effective_multiplier": 0.1,
            "billing_multiplier": 0.2,
            "billing_status": "discount",
            "billing_status_note": "限时折扣",
            "context_window": 1000000,
            "params": {"max_tokens": 100000}
          },
          {"name": "raccoon-internal", "visible": false, "description": "hidden"},
          {"name": "sn-kimi-k3", "visible": true, "description": "DUPLICATE"},
          {"name": "   ", "visible": true, "description": "blank id"},
          {
            "name": "sn-deepseek-v4-1-flash",
            "visible": true,
            "description": "",
            "billing_effective_multiplier": 0.25,
            "tags": ["reasoning"]
          }
        ]
      }
    ]
  }
}`

func TestParseCatalog(t *testing.T) {
	got := parseCatalog([]byte(catalogFixture))
	if len(got) != 4 {
		t.Fatalf("parseCatalog returned %d models, want 4 (chat only, dedup, visible, non-blank): %v", len(got), ids(got))
	}
	wantIDs := []string{"sn-kimi-k3", "sn-sensenova-6-8-flash", "sn-glm-5-3-flash", "sn-deepseek-v4-1-flash"}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Errorf("model[%d].ID = %q, want %q (remote order, first wins)", i, got[i].ID, want)
		}
	}
	for _, m := range got {
		if m.OwnedBy != "raccoon" {
			t.Errorf("%s: OwnedBy = %q", m.ID, m.OwnedBy)
		}
	}

	check := func(id, key string, want any) {
		t.Helper()
		for _, m := range got {
			if m.ID != id {
				continue
			}
			if m.Extra[key] != want {
				t.Errorf("%s: Extra[%q] = %#v, want %#v", id, key, m.Extra[key], want)
			}
			return
		}
		t.Errorf("%s not found", id)
	}
	check("sn-kimi-k3", "display_name", "Kimi-K3 · x1")
	check("sn-kimi-k3", "context_length", 1000000)
	check("sn-kimi-k3", "max_output_tokens", 100000)
	check("sn-kimi-k3", "vision", true)
	check("sn-kimi-k3", "billing_status", "normal")
	check("sn-sensenova-6-8-flash", "display_name", "SenseNova-6.8-Flash · 免费")
	check("sn-sensenova-6-8-flash", "context_length", 256000)
	check("sn-glm-5-3-flash", "display_name", "GLM-5-3-Flash · x0.2→x0.1")
	check("sn-glm-5-3-flash", "context_length", 1000000)
	check("sn-glm-5-3-flash", "billing_status_note", "限时折扣")
	check("sn-deepseek-v4-1-flash", "display_name", "sn-deepseek-v4-1-flash · x0.25")
	check("sn-deepseek-v4-1-flash", "reasoning", true)

	// No vision tag on the ones without one.
	for _, m := range got {
		if m.ID == "sn-glm-5-3-flash" {
			if _, ok := m.Extra["vision"]; ok {
				t.Error("sn-glm-5-3-flash must not claim vision support")
			}
		}
	}
}

func TestParseCatalogNeverFails(t *testing.T) {
	for _, body := range []string{
		``,
		`{`,
		`null`,
		`[]`,
		`{"code": 200003, "message": "please log in again"}`,
		`{"code": 0}`,
		`{"code": 0, "data": {}}`,
		`{"code": 0, "data": {"categories": null}}`,
		`{"code": 0, "data": {"categories": [{"type": "chat", "models": []}]}}`,
	} {
		if got := parseCatalog([]byte(body)); len(got) != 0 {
			t.Errorf("parseCatalog(%q) = %v, want empty (a catalogue failure must degrade, never panic)", body, ids(got))
		}
	}
}

func TestFallbackModels(t *testing.T) {
	got := fallbackModels()
	if len(got) != len(builtinModels) {
		t.Fatalf("fallbackModels returned %d, want %d", len(got), len(builtinModels))
	}
	want := map[string]string{
		"sn-sensenova-6-8-flash":      "SenseNova-6.8-Flash · 免费",
		"sn-sensenova-6-8-flash-lite": "SenseNova-6.8-Flash-Lite · 免费",
		"sn-glm-5-3":                  "GLM-5-3 · x0.75",
		"sn-kimi-k3":                  "Kimi-K3 · x1",
		"sn-glm-5-3-flash":            "GLM-5-3-Flash · x0.2→x0.1",
		"sn-deepseek-v4-1-flash":      "DeepSeek-V4.1-Flash · x0.25",
	}
	for _, m := range got {
		w, ok := want[m.ID]
		if !ok {
			t.Errorf("unexpected built-in model %q", m.ID)
			continue
		}
		if m.Extra["display_name"] != w {
			t.Errorf("%s: display_name = %v, want %q", m.ID, m.Extra["display_name"], w)
		}
		if m.Extra["fallback"] != true {
			t.Errorf("%s: must be marked fallback", m.ID)
		}
		if _, ok := core.ModelOutputLimit(m); !ok {
			t.Errorf("%s: built-in row must carry a usable max_output_tokens", m.ID)
		}
	}
}

func TestModelMaxOutputTokensDeclinesOnColdCache(t *testing.T) {
	c := newTestClient(t, t.TempDir(), `{"access_token": "tok"}`, nil)
	if n, ok := c.ModelMaxOutputTokens(nil, "sn-kimi-k3"); ok {
		t.Fatalf("a cold catalogue cache must DECLINE, got (%d, true)", n)
	}
	// The built-in table is available to Models(), but the limits provider
	// must not use it: the contract is "answer from a cache, decline when cold".
	ms, _ := c.Models(nil)
	if len(ms) == 0 {
		t.Fatal("Models() must still answer from the built-in table")
	}
	if _, ok := c.ModelMaxOutputTokens(nil, "sn-kimi-k3"); ok {
		t.Fatal("Models() alone must not warm the limits cache")
	}

	c.setModels([]core.Model{{
		ID:    "sn-kimi-k3",
		Extra: map[string]any{"max_output_tokens": 100000, "display_name": "Kimi-K3 · x1"},
	}})
	n, ok := c.ModelMaxOutputTokens(nil, "sn-kimi-k3")
	if !ok || n != 100000 {
		t.Fatalf("warm cache: got (%d, %v), want (100000, true)", n, ok)
	}
	if n, ok := c.ModelMaxOutputTokens(nil, "Kimi-K3 · x1"); !ok || n != 100000 {
		t.Fatalf("a display name must also resolve: got (%d, %v)", n, ok)
	}
	if _, ok := c.ModelMaxOutputTokens(nil, "sn-unknown"); ok {
		t.Fatal("an unknown model must decline")
	}
	if _, ok := c.ModelMaxOutputTokens(nil, "  "); ok {
		t.Fatal("an empty model must decline")
	}
}

func TestModelOutputLimitRejectsSillyValues(t *testing.T) {
	if _, ok := core.ModelOutputLimit(core.Model{Extra: map[string]any{"max_output_tokens": -1}}); ok {
		t.Error("a negative budget must be rejected")
	}
	if _, ok := core.ModelOutputLimit(core.Model{Extra: map[string]any{"max_output_tokens": json.Number("0")}}); ok {
		t.Error("a zero budget must be rejected")
	}
	if n, ok := core.ModelOutputLimit(core.Model{Extra: map[string]any{"max_output_tokens": json.Number("63999")}}); !ok || n != 63999 {
		t.Errorf("json.Number budget: got (%d, %v)", n, ok)
	}
}

func TestBuildChatBody(t *testing.T) {
	mt, temp := 4096, 0.3
	req := &core.ChatRequest{
		Model:       "sn-kimi-k3 · x1",
		Messages:    []core.Message{{Role: "user", Content: "hi"}},
		Tools:       []core.Tool{{Type: "function", Name: "get_weather", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice:  json.RawMessage(`"auto"`),
		MaxTokens:   &mt,
		Temperature: &temp,
		Options:     map[string]any{"reasoning_effort": "off"},
	}
	b := buildChatBody(req, "sn-kimi-k3", true)

	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if top["model"] != "sn-kimi-k3" {
		t.Errorf("model = %v", top["model"])
	}
	if top["stream"] != true {
		t.Errorf("stream = %v, want true", top["stream"])
	}
	if top["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens = %v", top["max_tokens"])
	}
	if top["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v", top["tool_choice"])
	}
	// tools MUST be at the TOP LEVEL: a nested/absent tools array makes the
	// model invent XML tool calls in prose.
	tools, ok := top["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools must be a top-level array, got %#v", top["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("tool type = %v", tool["type"])
	}
	fn, ok := tool["function"].(map[string]any)
	if !ok || fn["name"] != "get_weather" {
		t.Fatalf("tool function = %#v", tool["function"])
	}
	if _, ok := fn["parameters"]; !ok {
		t.Error("tool parameters must be forwarded")
	}
	eb, ok := top["extra_body"].(map[string]any)
	if !ok {
		t.Fatalf("extra_body missing: %#v", top["extra_body"])
	}
	th, _ := eb["thinking"].(map[string]any)
	if th["type"] != "disabled" {
		t.Errorf("reasoning effort off must send thinking.type=disabled, got %#v", eb)
	}

	// effort on => enabled; no effort => no extra_body at all.
	req.Options = map[string]any{"reasoning_effort": "on"}
	if got := extraBodyFor(reasoningEffort(req))["thinking"].(map[string]any)["type"]; got != "enabled" {
		t.Errorf("effort on => %v, want enabled", got)
	}
	req.Options = nil
	if eb := extraBodyFor(reasoningEffort(req)); eb != nil {
		t.Errorf("no effort must send no extra_body, got %#v", eb)
	}
	// ...and a top-level `thinking` is never invented.
	req.Options = map[string]any{"thinking": true}
	if got := extraBodyFor(reasoningEffort(req))["thinking"].(map[string]any)["type"]; got != "enabled" {
		t.Errorf("a boolean thinking option => %v, want enabled", got)
	}
}

func TestMessageContentParts(t *testing.T) {
	if got := messageContent(core.Message{Role: "user", Content: "hi"}); got != "hi" {
		t.Fatalf("plain content = %#v, want the string", got)
	}
	got := messageContent(core.Message{
		Role:    "user",
		Content: "look",
		Parts:   []core.ContentPart{{Type: "image_url", ImageURL: "https://x/y.png", Detail: "high"}},
	})
	parts, ok := got.([]map[string]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("multimodal content = %#v, want text + image parts", got)
	}
	if parts[0]["type"] != "text" || parts[0]["text"] != "look" {
		t.Errorf("part 0 = %#v", parts[0])
	}
	img, _ := parts[1]["image_url"].(map[string]any)
	if img["url"] != "https://x/y.png" || img["detail"] != "high" {
		t.Errorf("part 1 = %#v", parts[1])
	}
}

func TestModelsNeverBlocksAndFallsBack(t *testing.T) {
	c := newTestClient(t, t.TempDir(), `{"access_token": "tok"}`, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ms, err := c.Models(nil)
		if err != nil {
			t.Errorf("Models: %v", err)
		}
		if len(ms) != len(builtinModels) {
			t.Errorf("Models returned %d, want the %d built-in rows", len(ms), len(builtinModels))
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Models() must never block on the network")
	}
}

func TestRefreshModelsReturnsLastGoodOnFailure(t *testing.T) {
	// No reachable upstream: the refresh must still hand back a usable list.
	c := newTestClient(t, t.TempDir(), `{"access_token": "tok", "models_timeout": "50ms"}`, nil)
	got, err := c.RefreshModels(nil)
	if err == nil {
		t.Fatal("a catalogue fetch against an unreachable host must report an error")
	}
	if len(got) != len(builtinModels) {
		t.Fatalf("RefreshModels returned %d models alongside the error, want the built-in %d", len(got), len(builtinModels))
	}
}

func ids(list []core.Model) []string {
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.ID)
	}
	return out
}
