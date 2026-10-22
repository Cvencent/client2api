package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"client2api/internal/core"
)

type probeVendor struct {
	name     string
	window   int64
	refusal  string
	seens    []*core.ChatRequest
	refuseOn int
}

func (c *probeVendor) Name() string { return c.name }

func (c *probeVendor) Models(ctx context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "cn:space-bunny", OwnedBy: c.name, Extra: map[string]any{
		"context_length": c.window,
	}}}, nil
}

func (c *probeVendor) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	c.seens = append(c.seens, req)
	if c.refusal != "" && len(c.seens) == c.refuseOn {
		return nil, core.Fail(c.name, "", core.FailureContextWindow, http.StatusBadRequest, errors.New(c.refusal))
	}
	return &fakeStream{events: []core.Event{
		{Type: core.EventDelta, Delta: "ok"},
		{Type: core.EventUsage, Usage: &core.Usage{PromptTokens: estimateRequestTokens(req), CompletionTokens: 1}},
		{Type: core.EventDone, Finish: "stop"},
	}}, nil
}

func (c *probeVendor) Status(ctx context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true}
}

func probePathServer(t *testing.T, v *probeVendor, probe bool) (*http.Server, *UsageStore, *probeStore) {
	t.Helper()
	usage := NewUsageStore(0)
	probes := openProbeStore(filepath.Join(t.TempDir(), "probes.json"), t.Logf)
	s := newGuardServer(t, v, "", func(o *Options) {
		o.Usage = usage
		o.ProbeStore = probes
		o.ProbeBeforeTrim = probe
	})
	s.probes = probes
	return &http.Server{Handler: s.handler}, usage, probes
}

// probeBody builds a chat body of alternating turns whose content is CJK (one
// token per character), so the estimate is exact.  Several turns matter: a
// single oversized message can never be trimmed, so the guard would refuse it
// as impossible rather than exercising the trim path at all.
func probeBody(turns, perTurn int) string {
	msgs := make([]map[string]string, 0, turns)
	for i := 0; i < turns; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]string{
			"role":    role,
			"content": strings.Repeat("一", perTurn),
		})
	}
	b, _ := json.Marshal(map[string]any{
		"model":    "fake/cn:space-bunny",
		"messages": msgs,
		"stream":   false,
	})
	return string(b)
}

// probePromptTokens is the estimate of probeBody(30, 35000): about 1.05M.
const probeTurns = 30
const probePerTurn = 35000

func lastRecord(t *testing.T, usage *UsageStore) UsageRecord {
	t.Helper()
	recs := usage.Snapshot()
	if len(recs) == 0 {
		t.Fatal("no usage record was written")
	}
	return recs[len(recs)-1]
}

func TestProbeForwardsWholeAndRecordsTheRow(t *testing.T) {
	v := &probeVendor{name: "fake", window: 1_000_000}
	h, usage, _ := probePathServer(t, v, true)

	rr := chat(t, h, probeBody(probeTurns, probePerTurn))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	if len(v.seens) != 1 {
		t.Fatalf("vendor calls = %d, want 1", len(v.seens))
	}
	if got := estimateRequestTokens(v.seens[0]); got < 1_040_000 {
		t.Fatalf("the probe was trimmed before sending: estimate = %d", got)
	}
	rec := lastRecord(t, usage)
	if !rec.Probed {
		t.Fatal("row must be marked probed")
	}
	if rec.ProbedFromTokens < 1_040_000 {
		t.Fatalf("ProbedFromTokens = %d, want the forwarded estimate", rec.ProbedFromTokens)
	}
	if rec.Trimmed {
		t.Fatal("a forwarded-whole probe must not also be marked trimmed")
	}
}

func TestProbeRefusalStillTrimsAndRetriesOnce(t *testing.T) {
	v := &probeVendor{
		name:     "fake",
		window:   1_000_000,
		refusal:  "prompt is too long: 1100000 tokens > 1000000 maximum",
		refuseOn: 1,
	}
	h, usage, probes := probePathServer(t, v, true)

	rr := chat(t, h, probeBody(probeTurns, probePerTurn))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	if len(v.seens) != 2 {
		t.Fatalf("vendor calls = %d, want exactly 2 (probe + one retry)", len(v.seens))
	}
	rec := lastRecord(t, usage)
	if !rec.Trimmed {
		t.Fatal("the retry must be recorded as a trim")
	}
	if got, ok := probes.get("fake", "cn:space-bunny"); !ok || got.Outcome != probeOutcomeRefused {
		t.Fatalf("probe verdict = %+v ok=%v, want refused", got, ok)
	}
}

func TestProbeDisabledKeepsTrimming(t *testing.T) {
	v := &probeVendor{name: "fake", window: 1_000_000}
	h, usage, _ := probePathServer(t, v, false)

	rr := chat(t, h, probeBody(probeTurns, probePerTurn))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	rec := lastRecord(t, usage)
	if rec.Probed {
		t.Fatal("probe disabled must not mark the row probed")
	}
	if !rec.Trimmed {
		t.Fatal("probe disabled must keep the old trim path")
	}
}

func TestProbeAcceptedFloorPreventsASecondTrim(t *testing.T) {
	v := &probeVendor{name: "fake", window: 1_000_000}
	h, usage, probes := probePathServer(t, v, true)

	if rr := chat(t, h, probeBody(probeTurns, probePerTurn)); rr.Code != http.StatusOK {
		t.Fatalf("first status = %d body = %s", rr.Code, rr.Body.String())
	}
	rec, ok := probes.get("fake", "cn:space-bunny")
	if !ok || rec.Outcome != probeOutcomeAccepted {
		t.Fatalf("after the first call the probe = %+v ok=%v, want accepted", rec, ok)
	}

	if rr := chat(t, h, probeBody(probeTurns, probePerTurn)); rr.Code != http.StatusOK {
		t.Fatalf("second status = %d body = %s", rr.Code, rr.Body.String())
	}
	if len(v.seens) != 2 {
		t.Fatalf("vendor calls = %d, want 2", len(v.seens))
	}
	if got := estimateRequestTokens(v.seens[1]); got < 1_040_000 {
		t.Fatalf("second request was trimmed despite an accepted floor: %d", got)
	}
	rec2 := lastRecord(t, usage)
	if rec2.Probed {
		t.Fatal("an already-settled model must not be marked probing again")
	}
	if rec2.Trimmed {
		t.Fatal("an already-settled model must not be trimmed")
	}
}
