package gateway

import (
	"io"
	"log"
	"math"
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/modelmeta"
)

func TestCostForUsageUsesCacheReadPrice(t *testing.T) {
	price := ModelPrice{
		InputPerMillion:     10,
		OutputPerMillion:    30,
		CacheReadPerMillion: 1,
		HasCacheRead:        true,
	}
	got := CostForUsage(price, &core.Usage{
		PromptTokens:     1000,
		CachedTokens:     400,
		CompletionTokens: 200,
	})
	want := 0.0124 // (600*10 + 400*1 + 200*30) / 1e6
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("CostForUsage() = %v, want %v", got, want)
	}
}

func TestCostForUsageFallsBackToInputPriceWithoutCacheRate(t *testing.T) {
	price := ModelPrice{InputPerMillion: 2, OutputPerMillion: 8}
	got := CostForUsage(price, &core.Usage{
		PromptTokens:     1000,
		CachedTokens:     400,
		CompletionTokens: 100,
	})
	want := 0.0028 // (1000*2 + 100*8) / 1e6
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("CostForUsage() = %v, want %v", got, want)
	}
}

func TestUsageReportCarriesCacheAndCost(t *testing.T) {
	s := NewUsageStore(8)
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	s.Add(at, "zcode", "cn", "a1", "zcode/glm-5.3", UsageDelta{
		PromptTokens:     1000,
		HasPromptTokens:  true,
		CachedTokens:     400,
		HasCachedTokens:  true,
		CompletionTokens: 200,
		HasCompletion:    true,
		TotalTokens:      1200,
		HasTotal:         true,
		Cost:             0.0126,
		HasCost:          true,
	}, true)

	rep := s.UsageReport(0)
	if !rep.Totals.HasCachedTokens || rep.Totals.CachedTokens != 400 {
		t.Fatalf("totals cache = has:%v value:%d, want has:true value:400", rep.Totals.HasCachedTokens, rep.Totals.CachedTokens)
	}
	if !rep.Totals.HasCost || math.Abs(rep.Totals.Cost-0.0126) > 1e-12 {
		t.Fatalf("totals cost = has:%v value:%v, want has:true value:0.0126", rep.Totals.HasCost, rep.Totals.Cost)
	}
	if len(rep.ByModel) != 1 || !rep.ByModel[0].HasCost || rep.ByModel[0].Cost != 0.0126 {
		t.Fatalf("model row = %+v, want the same cache/cost totals", rep.ByModel)
	}
	if len(rep.Series) != 1 {
		t.Fatalf("series points = %d, want 1", len(rep.Series))
	}
	p := rep.Series[0]
	if !p.HasCachedTokens || p.CachedTokens != 400 || !p.HasCost || math.Abs(p.Cost-0.0126) > 1e-12 {
		t.Fatalf("series point = %+v, want cache 400 and cost 0.0126", p)
	}
}

func TestUsageRecordPriceUsageLeavesUnknownUnpriced(t *testing.T) {
	rec := UsageRecord{PromptTokens: 100, CompletionTokens: 50}
	rec.priceUsage(ModelPrice{InputPerMillion: 1, OutputPerMillion: 2}, false)
	if rec.HasCost || rec.Cost != 0 {
		t.Fatalf("unknown price stored a cost: %+v", rec)
	}
	rec.priceUsage(ModelPrice{InputPerMillion: 1, OutputPerMillion: 2}, true)
	if !rec.HasCost || math.Abs(rec.Cost-0.0002) > 1e-12 {
		t.Fatalf("known price cost = has:%v value:%v, want true/0.0002", rec.HasCost, rec.Cost)
	}
}

func TestChatRecordsOfficialAndManualModelCost(t *testing.T) {
	events := []core.Event{
		{Type: core.EventDelta, Delta: "hello"},
		{Type: core.EventUsage, Usage: &core.Usage{PromptTokens: 1000, CachedTokens: 400, CompletionTokens: 200, TotalTokens: 1200}},
		{Type: core.EventDone, Finish: "stop"},
	}
	tests := []struct {
		name    string
		client  *testClient
		pricing func(*modelmeta.OverrideStore)
		body    string
		want    float64
	}{
		{
			name:   "official default",
			client: &testClient{name: "openai", catalogue: []core.Model{{ID: "gpt-5.4"}}, events: events},
			body:   `{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}`,
			want:   0.03312, // (600*18 + 400*1.8 + 200*108) / 1e6
		},
		{
			name:   "manual override",
			client: &testClient{name: "zcode", events: events},
			pricing: func(store *modelmeta.OverrideStore) {
				if err := store.SetPrice("zcode", "m1", 10, 30, 1, true); err != nil {
					t.Fatal(err)
				}
			},
			body: bufferedBody,
			want: 0.0124, // (600*10 + 400*1 + 200*30) / 1e6
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, err := modelmeta.OpenOverrideStore("")
			if err != nil {
				t.Fatal(err)
			}
			if tc.pricing != nil {
				tc.pricing(store)
			}
			usage := NewUsageStore(10)
			reg := core.NewRegistry()
			reg.Add(tc.client)
			srv := NewServer(Options{
				Registry:       reg,
				Version:        "test",
				Logger:         log.New(io.Discard, "", 0),
				Usage:          usage,
				ModelOverrides: store,
			})

			rec := chat(t, srv, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
			got := usage.Snapshot()
			if len(got) != 1 {
				t.Fatalf("usage records = %d, want 1", len(got))
			}
			if !got[0].HasCost || math.Abs(got[0].Cost-tc.want) > 1e-12 {
				t.Fatalf("cost = has:%v value:%v, want has:true value:%v", got[0].HasCost, got[0].Cost, tc.want)
			}
		})
	}
}
func TestUsageReportCountsUnpricedSuccessfulCalls(t *testing.T) {
	s := NewUsageStore(8)
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	s.Add(at, "unknown", "cn", "a1", "unknown/m", UsageDelta{
		PromptTokens:    100,
		HasPromptTokens: true,
	}, true)
	s.Add(at, "known", "cn", "a2", "known/m", UsageDelta{
		PromptTokens:    100,
		HasPromptTokens: true,
		Cost:            0.01,
		HasCost:         true,
	}, true)
	s.Add(at, "failed", "cn", "a3", "failed/m", UsageDelta{
		PromptTokens:    100,
		HasPromptTokens: true,
	}, false)

	rep := s.UsageReport(0)
	if rep.Totals.UnpricedRequests != 1 {
		t.Fatalf("unpriced requests = %d, want 1", rep.Totals.UnpricedRequests)
	}
	if !rep.Totals.HasCost || math.Abs(rep.Totals.Cost-0.01) > 1e-12 {
		t.Fatalf("known cost = has:%v value:%v, want true/0.01", rep.Totals.HasCost, rep.Totals.Cost)
	}
}
