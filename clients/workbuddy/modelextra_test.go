package workbuddy

import (
	"encoding/json"
	"testing"
	"time"
)

// --- modelExtra -------------------------------------------------------------

func TestWorkbuddyCreditsPrefixNormalisesTheUpstreamSpelling(t *testing.T) {
	// The upstream is not consistent about this field: the same multiplier
	// arrives with the unit, without it, and as a zero.  The prefix is what the
	// panel shows, so the unit must not be doubled and a bare unit must not
	// become a multiplier of nothing.
	tests := []struct{ raw, want string }{
		{"x0.05 credits", "[x0.05 credit]"},
		{"x0.29", "[x0.29 credit]"},
		{"x0.00 credits", "[x0.00 credit]"},
		{"  x1 credits  ", "[x1 credit]"},
		{"", ""},
		{"credits", ""},
		{"   ", ""},
	}
	for _, tc := range tests {
		if got := creditsPrefix(tc.raw); got != tc.want {
			t.Errorf("creditsPrefix(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestWorkbuddyModelExtraCarriesTheRateAndTheCapabilities(t *testing.T) {
	factor := 0.5
	mi := ModelInfo{
		ID:                 "glm-5.2",
		Name:               "GLM 5.2",
		ContextWindow:      200000,
		MaxTokens:          64000,
		Efforts:            []string{"low", "medium", "high"},
		DefaultEffort:      "medium",
		Description:        "通用对话模型",
		Credits:            "x0.29",
		Tags:               []string{"chat"},
		Vendor:             "zhipu",
		IsDefault:          true,
		SupportsReasoning:  true,
		SupportsToolCall:   true,
		SupportsImages:     true,
		CanDisableThinking: true,
		MaxAllowedSize:     10485760,
		ReasoningEffort:    "medium",
		ReasoningSummary:   "auto",
		PromoFactor:        &factor,
		PromoCredits:       "x0.15",
		PromoLabel:         "夜间五折",
		PromoNote:          "23:00-07:50 生效",
	}
	extra := modelExtra(mi)

	// The multiplier is the headline fact, and it is also folded into the
	// description so a consumer that renders only that still sees the price.
	if extra["credits"] != "x0.29" {
		t.Errorf("credits = %v, want the list price", extra["credits"])
	}
	if extra["description"] != "[x0.29 credit] 通用对话模型" {
		t.Errorf("description = %v, want the credits prefix in front of the text", extra["description"])
	}
	if extra["promo_credits"] != "x0.15" || extra["promo_label"] != "夜间五折" || extra["promo_note"] != "23:00-07:50 生效" {
		t.Errorf("promo trio = %v/%v/%v, want the effective campaign",
			extra["promo_credits"], extra["promo_label"], extra["promo_note"])
	}
	if got, ok := extra["promo_factor"].(float64); !ok || got != 0.5 {
		t.Errorf("promo_factor = %v, want the 0.5 discount factor", extra["promo_factor"])
	}

	for key, want := range map[string]any{
		"name":              "GLM 5.2",
		"vendor":            "zhipu",
		"reasoning_effort":  "medium",
		"reasoning_summary": "auto",
		"context_length":    int64(200000),
		"max_output_tokens": int64(64000),
		"max_allowed_size":  int64(10485760),
		"is_default":        true,
		"supports_images":   true,
	} {
		if got := extra[key]; got != want {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}

	for _, key := range []string{"supports_reasoning", "supports_tool_call", "can_disable_thinking"} {
		if extra[key] != true {
			t.Errorf("%s = %v, want true", key, extra[key])
		}
	}
	if _, ok := extra["only_reasoning"]; ok {
		t.Error("only_reasoning appeared for a model that is not reasoning-only")
	}

	if got, ok := extra["tags"].([]string); !ok || len(got) != 1 || got[0] != "chat" {
		t.Errorf("tags = %#v, want [chat]", extra["tags"])
	}
	if got, ok := extra["reasoning_supported_efforts"].([]string); !ok || len(got) != 3 {
		t.Errorf("reasoning_supported_efforts = %#v, want the three-entry ladder", extra["reasoning_supported_efforts"])
	}
	if extra["reasoning_default_effort"] != "medium" {
		t.Errorf("reasoning_default_effort = %v, want medium", extra["reasoning_default_effort"])
	}
}

// The keys this module served before the reference spellings were added are
// still published: the on-disk catalogue cache and older panel builds read them.
func TestWorkbuddyModelExtraKeepsTheOlderSpellings(t *testing.T) {
	mi := ModelInfo{
		ID:            "glm-5.2",
		ContextWindow: 200000,
		MaxTokens:     64000,
		Efforts:       []string{"low", "high"},
	}
	extra := modelExtra(mi)
	if extra["context_window"] != int64(200000) || extra["context_length"] != int64(200000) {
		t.Errorf("context spellings = %v/%v, want both", extra["context_window"], extra["context_length"])
	}
	if extra["max_tokens"] != int64(64000) || extra["max_output_tokens"] != int64(64000) {
		t.Errorf("output spellings = %v/%v, want both", extra["max_tokens"], extra["max_output_tokens"])
	}
	if extra["reasoning_efforts"] == nil || extra["reasoning_supported_efforts"] == nil {
		t.Error("the effort ladder is missing one of its two spellings")
	}
}

// The GPT-6 catalogue's 1,050,000 value is above the backend's hard request
// ceiling. Advertising it to Codex delays compaction until the next turn is
// already too large, so the module must expose the real serving limit.
func TestWorkbuddyModelExtraCapsTheContextAtTheServingLimit(t *testing.T) {
	extra := modelExtra(ModelInfo{ID: "gpt-5.5", ContextWindow: 1050000})
	for _, key := range []string{"context_length", "context_window"} {
		if got := extra[key]; got != int64(1048576) {
			t.Fatalf("%s = %v, want 1048576", key, got)
		}
	}

	small := modelExtra(ModelInfo{ID: "gpt-5.3-codex", ContextWindow: 400000})
	for _, key := range []string{"context_length", "context_window"} {
		if got := small[key]; got != int64(400000) {
			t.Fatalf("%s = %v, want the smaller upstream value unchanged", key, got)
		}
	}
}

// "The upstream did not say" and "the upstream said zero" are different facts: a
// zero must never be published as a real limit, and a model with no promotion
// must not grow promo keys that would make the panel draw a discount.
func TestWorkbuddyModelExtraOmitsWhatUpstreamDidNotSay(t *testing.T) {
	extra := modelExtra(ModelInfo{ID: "glm-5.2", Name: "GLM 5.2"})
	if len(extra) != 1 || extra["name"] != "GLM 5.2" {
		t.Fatalf("extra = %v, want the name and nothing else", extra)
	}
	for _, key := range []string{
		"context_length", "context_window", "max_output_tokens", "max_tokens",
		"reasoning_supported_efforts", "reasoning_efforts", "reasoning_default_effort",
		"credits", "description", "promo_factor", "promo_credits", "promo_label", "promo_note",
		"tags", "vendor", "max_allowed_size", "reasoning_effort", "reasoning_summary",
		"is_default", "supports_reasoning", "supports_tool_call", "only_reasoning",
		"supports_images", "can_disable_thinking",
	} {
		if v, ok := extra[key]; ok {
			t.Errorf("%s = %#v, want the key absent when upstream said nothing", key, v)
		}
	}
}

// A credits-only model still gets the multiplier, with the prefix standing alone.
func TestWorkbuddyModelExtraPrefixesACreditsOnlyModel(t *testing.T) {
	extra := modelExtra(ModelInfo{ID: "glm-5.2", Credits: "x0.00 credits"})
	if extra["description"] != "[x0.00 credit]" {
		t.Errorf("description = %v, want the bare prefix", extra["description"])
	}
}

// --- promotion scheduling ---------------------------------------------------

func promoAt(t *testing.T, raw string) v3ModelPromotion {
	t.Helper()
	var p v3ModelPromotion
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("promotion fixture: %v", err)
	}
	return p
}

// promoTime builds a wall-clock instant in the promotion zone, which is what the
// upstream schedules are quoted in.
func promoTime(h, m int) time.Time {
	return time.Date(2026, 10, 1, h, m, 0, 0, promoZone)
}

func TestWorkbuddyPromoClockRejectsBadValues(t *testing.T) {
	tests := []struct {
		in   string
		want int
		ok   bool
	}{
		{"00:00", 0, true},
		{"09:30", 570, true},
		{"23:59", 1439, true},
		{" 7:05 ", 425, true},
		{"24:00", 1440, true},
		{"", -1, false},
		{"9", -1, false},
		{"09:60", -1, false},
		{"25:00", -1, false},
		{"-1:00", -1, false},
		{"ab:cd", -1, false},
		{"09:00:00", -1, false},
	}
	for _, tc := range tests {
		got, ok := promoClock(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("promoClock(%q) = %d,%v want %d,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestWorkbuddyPromoActiveHonoursTheDailyWindow(t *testing.T) {
	p := promoAt(t, `{"enabled":true,"schedule":{"daily":[{"start":"09:00","end":"18:00"}]}}`)
	tests := []struct {
		at   time.Time
		want bool
	}{
		{promoTime(8, 59), false},
		{promoTime(9, 0), true},
		{promoTime(12, 0), true},
		{promoTime(17, 59), true},
		{promoTime(18, 0), false}, // the end bound is exclusive
		{promoTime(23, 0), false},
	}
	for _, tc := range tests {
		if got := promoActive(&p, tc.at); got != tc.want {
			t.Errorf("promoActive at %s = %v, want %v", tc.at.Format("15:04"), got, tc.want)
		}
	}
}

// A window that wraps past midnight is the shape the live night campaign uses.
func TestWorkbuddyPromoActiveHandlesAWindowAcrossMidnight(t *testing.T) {
	p := promoAt(t, `{"enabled":true,"schedule":{"daily":[{"start":"23:00","end":"07:50"}]}}`)
	tests := []struct {
		at   time.Time
		want bool
	}{
		{promoTime(22, 59), false},
		{promoTime(23, 0), true},
		{promoTime(23, 59), true},
		{promoTime(0, 0), true},
		{promoTime(7, 49), true},
		{promoTime(7, 50), false},
		{promoTime(12, 0), false},
	}
	for _, tc := range tests {
		if got := promoActive(&p, tc.at); got != tc.want {
			t.Errorf("promoActive at %s = %v, want %v", tc.at.Format("15:04"), got, tc.want)
		}
	}
}

func TestWorkbuddyPromoActiveHonoursTheValidityRange(t *testing.T) {
	past := "2026-01-01T00:00:00Z"
	future := "2027-01-01T00:00:00Z"
	now := promoTime(12, 0)

	expired := promoAt(t, `{"enabled":true,"schedule":{"validUntil":"`+past+`"}}`)
	if promoActive(&expired, now) {
		t.Error("an expired promotion is still active")
	}
	notYet := promoAt(t, `{"enabled":true,"schedule":{"validFrom":"`+future+`"}}`)
	if promoActive(&notYet, now) {
		t.Error("a promotion that has not started is active")
	}
	inRange := promoAt(t, `{"enabled":true,"schedule":{"validFrom":"`+past+`","validUntil":"`+future+`"}}`)
	if !promoActive(&inRange, now) {
		t.Error("a promotion inside its validity range is inactive")
	}
	// A malformed range must not silently disable the campaign.
	broken := promoAt(t, `{"enabled":true,"schedule":{"validFrom":"not-a-time"}}`)
	if !promoActive(&broken, now) {
		t.Error("an unparseable validFrom disabled the promotion")
	}
}

func TestWorkbuddyPromoActiveDefaults(t *testing.T) {
	now := promoTime(12, 0)
	if promoActive(nil, now) {
		t.Error("a nil promotion is active")
	}
	off := promoAt(t, `{"enabled":false}`)
	if promoActive(&off, now) {
		t.Error("a disabled promotion is active")
	}
	noSchedule := promoAt(t, `{"enabled":true}`)
	if !promoActive(&noSchedule, now) {
		t.Error("a promotion with no schedule must run all day")
	}
	emptyDaily := promoAt(t, `{"enabled":true,"schedule":{"daily":[]}}`)
	if !promoActive(&emptyDaily, now) {
		t.Error("an empty daily list must mean all day, not never")
	}
	// Every window malformed: nothing is in force, but the run must not panic.
	bad := promoAt(t, `{"enabled":true,"schedule":{"daily":[{"start":"nope","end":"18:00"},{"start":"09:00","end":"nope"}]}}`)
	if promoActive(&bad, now) {
		t.Error("a schedule of unparseable windows is active")
	}
}

// --- applying promotions to the catalogue -----------------------------------

func promosFromJSON(t *testing.T, raw string) []v3ModelPromotion {
	t.Helper()
	var out []v3ModelPromotion
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("promotion fixture: %v", err)
	}
	return out
}

// The live catalogue runs a badge-only campaign by day and a half-price one at
// night, told apart by priority and the daily window.  The night one wins when
// it is in force, and the day one must not leak into the night.
func TestWorkbuddyApplyModelPromotionsPicksTheActiveHighestPriority(t *testing.T) {
	const fixture = `[
	  {"enabled":true,"priority":50,"modelIds":["glm-5.2"],
	   "badge":{"label":"错峰优惠"},
	   "schedule":{"daily":[{"start":"09:00","end":"18:00"}]}},
	  {"enabled":true,"priority":100,"modelIds":["glm-5.2"],
	   "discount":{"discountedCredits":"x0.15","factor":0.5},
	   "badge":{"label":"夜间五折"},"hover":{"textZh":"23:00-07:50 生效"},
	   "schedule":{"daily":[{"start":"23:00","end":"07:50"}]}}
	]`
	promos := promosFromJSON(t, fixture)

	atNoon := map[string]ModelInfo{"glm-5.2": {ID: "glm-5.2"}}
	applyModelPromotionsAt(atNoon, promos, promoTime(12, 0))
	noon := atNoon["glm-5.2"]
	if noon.PromoLabel != "错峰优惠" {
		t.Errorf("noon label = %q, want the badge-only campaign", noon.PromoLabel)
	}
	if noon.PromoFactor != nil || noon.PromoCredits != "" {
		t.Errorf("noon price = %v/%q, want no discount at noon", noon.PromoFactor, noon.PromoCredits)
	}

	atNight := map[string]ModelInfo{"glm-5.2": {ID: "glm-5.2"}}
	applyModelPromotionsAt(atNight, promos, promoTime(2, 0))
	night := atNight["glm-5.2"]
	if night.PromoLabel != "夜间五折" {
		t.Errorf("night label = %q, want the night campaign", night.PromoLabel)
	}
	if night.PromoFactor == nil || *night.PromoFactor != 0.5 {
		t.Errorf("night factor = %v, want 0.5", night.PromoFactor)
	}
	if night.PromoCredits != "x0.15" || night.PromoNote != "23:00-07:50 生效" {
		t.Errorf("night price/note = %q/%q, want the discounted pair", night.PromoCredits, night.PromoNote)
	}

	// Neither window is in force at 20:00, so the entry must stay untouched.
	between := map[string]ModelInfo{"glm-5.2": {ID: "glm-5.2"}}
	applyModelPromotionsAt(between, promos, promoTime(20, 0))
	if got := between["glm-5.2"]; got.PromoLabel != "" || got.PromoFactor != nil {
		t.Errorf("20:00 entry = %+v, want no promotion", got)
	}
}

// A campaign that names a model outside this catalogue (a same-named global
// variant, say) must be skipped rather than creating an entry.
func TestWorkbuddyApplyModelPromotionsIgnoresModelsOutsideTheCatalogue(t *testing.T) {
	promos := promosFromJSON(t, `[{"enabled":true,"priority":1,"modelIds":["absent"],
	  "discount":{"discountedCredits":"x0.01","factor":0.1}}]`)
	byID := map[string]ModelInfo{"glm-5.2": {ID: "glm-5.2"}}
	applyModelPromotionsAt(byID, promos, promoTime(12, 0))
	if len(byID) != 1 {
		t.Fatalf("catalogue grew to %d entries, want 1", len(byID))
	}
	if byID["glm-5.2"].PromoLabel != "" {
		t.Error("an unrelated model picked up a promotion")
	}
}

// Equal priorities keep the earlier campaign, which is what the reference does
// and what makes the result independent of map iteration order.
func TestWorkbuddyApplyModelPromotionsIsStableOnATie(t *testing.T) {
	promos := promosFromJSON(t, `[
	  {"enabled":true,"priority":7,"modelIds":["glm-5.2"],"badge":{"label":"first"}},
	  {"enabled":true,"priority":7,"modelIds":["glm-5.2"],"badge":{"label":"second"}}
	]`)
	for i := 0; i < 20; i++ {
		byID := map[string]ModelInfo{"glm-5.2": {ID: "glm-5.2"}}
		applyModelPromotionsAt(byID, promos, promoTime(12, 0))
		if got := byID["glm-5.2"].PromoLabel; got != "first" {
			t.Fatalf("run %d label = %q, want the earlier campaign", i, got)
		}
	}
}

func TestWorkbuddyApplyModelPromotionsHandlesEmptyInputs(t *testing.T) {
	byID := map[string]ModelInfo{"glm-5.2": {ID: "glm-5.2"}}
	applyModelPromotionsAt(byID, nil, promoTime(12, 0))
	if byID["glm-5.2"].PromoLabel != "" {
		t.Error("a nil promotion list changed the catalogue")
	}
	applyModelPromotionsAt(nil, promosFromJSON(t, `[{"enabled":true}]`), promoTime(12, 0))
	applyModelPromotionsAt(map[string]ModelInfo{}, promosFromJSON(t, `[{"enabled":true}]`), promoTime(12, 0))
}

// The /v3/config envelope is polymorphic upstream, and the campaign list has been
// seen both as a bare array and as an object keyed by campaign id.  Guessing
// wrong would fail the whole parse and take the catalogue down with it, so both
// shapes are resolved — and an unrecognised one costs only the decoration.
func TestWorkbuddyParseModelPromotionsAcceptsBothEnvelopes(t *testing.T) {
	asList := parseModelPromotions(json.RawMessage(`[
	  {"enabled":true,"priority":1,"modelIds":["a"]},
	  {"enabled":true,"priority":2,"modelIds":["b"]}
	]`))
	if len(asList) != 2 || asList[0].Priority != 1 || asList[1].Priority != 2 {
		t.Fatalf("array envelope = %+v, want the two campaigns in wire order", asList)
	}

	// A map is flattened in sorted-key order so the tie-break stays stable.
	asMap := parseModelPromotions(json.RawMessage(`{
	  "zzz": {"enabled":true,"priority":9,"modelIds":["z"]},
	  "aaa": {"enabled":true,"priority":9,"modelIds":["a"]}
	}`))
	if len(asMap) != 2 || asMap[0].ModelIDs[0] != "a" || asMap[1].ModelIDs[0] != "z" {
		t.Fatalf("object envelope = %+v, want sorted-key order", asMap)
	}

	for _, raw := range []string{"", "null", `"nope"`, "{}", "[1,2]"} {
		if got := parseModelPromotions(json.RawMessage(raw)); len(got) != 0 {
			t.Errorf("parseModelPromotions(%q) = %+v, want nothing", raw, got)
		}
	}
}
