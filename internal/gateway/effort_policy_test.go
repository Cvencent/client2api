package gateway

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"testing"

	"client2api/internal/core"
	"client2api/internal/modelmeta"
)

type effortPolicyClient struct {
	*testClient
	efforts       []string
	defaultEffort string
	ok            bool
	calls         int
}

func (c *effortPolicyClient) ModelEfforts(context.Context, string) ([]string, string, bool) {
	c.calls++
	return append([]string(nil), c.efforts...), c.defaultEffort, c.ok
}

func newEffortPolicyServer(t *testing.T, c core.Client, store *modelmeta.OverrideStore) *http.Server {
	t.Helper()
	reg := core.NewRegistry()
	reg.Add(c)
	return NewServer(Options{
		Registry:       reg,
		Version:        "test",
		Logger:         log.New(io.Discard, "", 0),
		ModelOverrides: store,
	})
}

func TestElevateTopPromotesXHighToTheModelsHighestTier(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetElevateTop("mod", "m1", true); err != nil {
		t.Fatal(err)
	}
	base := &testClient{name: "mod", catalogue: []core.Model{{ID: "m1"}}}
	client := &effortPolicyClient{
		testClient:    base,
		efforts:       []string{"low", "high", "xhigh", "max"},
		defaultEffort: "high",
		ok:            true,
	}
	srv := newEffortPolicyServer(t, client, store)

	body := `{"model":"m1","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`
	if rec := chat(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if base.seen == nil {
		t.Fatal("the request never reached the module")
	}
	if got := base.seen.Options["reasoning_effort"]; got != "max" {
		t.Fatalf("module reasoning_effort = %v, want max", got)
	}
	if !base.seen.EffortFinal {
		t.Fatal("a promoted level was not marked final, so the module can snap it back down")
	}
}

func TestElevateTopStaysOffByDefault(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	base := &testClient{name: "mod", catalogue: []core.Model{{ID: "m1"}}}
	client := &effortPolicyClient{
		testClient: base,
		efforts:    []string{"low", "high", "xhigh", "max"},
		ok:         true,
	}
	srv := newEffortPolicyServer(t, client, store)

	body := `{"model":"m1","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`
	if rec := chat(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := base.seen.Options["reasoning_effort"]; got != "xhigh" {
		t.Fatalf("module reasoning_effort = %v, want xhigh while the toggle is off", got)
	}
	if base.seen.EffortFinal {
		t.Fatal("an unchanged level was marked final")
	}
}

func TestElevateTopLeavesAModelWithoutAHigherTierAlone(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetElevateTop("mod", "m1", true); err != nil {
		t.Fatal(err)
	}
	base := &testClient{name: "mod", catalogue: []core.Model{{ID: "m1"}}}
	client := &effortPolicyClient{
		testClient: base,
		efforts:    []string{"low", "high", "xhigh"},
		ok:         true,
	}
	srv := newEffortPolicyServer(t, client, store)

	body := `{"model":"m1","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`
	if rec := chat(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := base.seen.Options["reasoning_effort"]; got != "xhigh" {
		t.Fatalf("module reasoning_effort = %v, want xhigh when xhigh is already the top", got)
	}
}

func TestElevateTopDoesNotTouchANonTopRequest(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetElevateTop("mod", "m1", true); err != nil {
		t.Fatal(err)
	}
	base := &testClient{name: "mod", catalogue: []core.Model{{ID: "m1"}}}
	client := &effortPolicyClient{
		testClient: base,
		efforts:    []string{"low", "high", "xhigh", "max"},
		ok:         true,
	}
	srv := newEffortPolicyServer(t, client, store)

	body := `{"model":"m1","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`
	if rec := chat(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := base.seen.Options["reasoning_effort"]; got != "high" {
		t.Fatalf("module reasoning_effort = %v, want high unchanged", got)
	}
}

func TestElevateTopUsesTheEmbeddedModelTable(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetElevateTop("workbuddy", "cn:deepseek-v4-flash", true); err != nil {
		t.Fatal(err)
	}
	base := &testClient{name: "workbuddy", catalogue: []core.Model{{ID: "cn:deepseek-v4-flash"}}}
	srv := newEffortPolicyServer(t, base, store)

	body := `{"model":"cn:deepseek-v4-flash","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`
	if rec := chat(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := base.seen.Options["reasoning_effort"]; got != "max" {
		t.Fatalf("module reasoning_effort = %v, want max from the embedded table", got)
	}
}

func TestElevateTopManualVocabularyOverridesModuleFacts(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetEfforts("mod", "m1", []string{"low", "high", "xhigh", "max"}, "high"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetElevateTop("mod", "m1", true); err != nil {
		t.Fatal(err)
	}
	base := &testClient{name: "mod", catalogue: []core.Model{{ID: "m1"}}}
	client := &effortPolicyClient{
		testClient: base,
		efforts:    []string{"low", "high", "xhigh"},
		ok:         true,
	}
	srv := newEffortPolicyServer(t, client, store)

	body := `{"model":"m1","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`
	if rec := chat(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := base.seen.Options["reasoning_effort"]; got != "max" {
		t.Fatalf("module reasoning_effort = %v, want the manual max", got)
	}
}

func TestUsageRowShowsBothRequestedAndSentReasoningEffort(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetElevateTop("mod", "m1", true); err != nil {
		t.Fatal(err)
	}
	base := &testClient{name: "mod", catalogue: []core.Model{{ID: "m1"}}, events: usageEvents(1, 1)}
	client := &effortPolicyClient{
		testClient: base,
		efforts:    []string{"low", "high", "xhigh", "max"},
		ok:         true,
	}
	usage := NewUsageStore(8)
	reg := core.NewRegistry()
	reg.Add(client)
	srv := NewServer(Options{
		Registry:       reg,
		Version:        "test",
		Logger:         log.New(io.Discard, "", 0),
		ModelOverrides: store,
		Usage:          usage,
	})

	body := `{"model":"m1","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`
	if rec := chat(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rows := usage.Snapshot()
	if len(rows) != 1 {
		t.Fatalf("recorded %d usage rows, want 1", len(rows))
	}
	if rows[0].ReasoningEffort != "xhigh" || rows[0].SentReasoningEffort != "max" {
		t.Fatalf("usage efforts = requested %q sent %q, want xhigh and max", rows[0].ReasoningEffort, rows[0].SentReasoningEffort)
	}
}

func TestElevateTopResetsTheOriginalLevelForTheNextCandidate(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetElevateTop("alpha", "m1", true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetElevateTop("beta", "m1", true); err != nil {
		t.Fatal(err)
	}
	alphaBase := &testClient{
		name:    "alpha",
		chatErr: core.Fail("alpha", "a1", core.FailureUpstream, http.StatusBadGateway, errors.New("boom")),
	}
	betaBase := &testClient{name: "beta", events: usageEvents(1, 1)}
	alpha := &effortPolicyClient{
		testClient: alphaBase,
		efforts:    []string{"low", "high", "xhigh", "max"},
		ok:         true,
	}
	beta := &effortPolicyClient{
		testClient: betaBase,
		efforts:    []string{"low", "high", "xhigh"},
		ok:         true,
	}
	reg := core.NewRegistry()
	reg.Add(alpha)
	reg.Add(beta)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"alpha": {Priority: 1},
		"beta":  {Priority: 2},
	})
	srv := NewServer(Options{
		Registry:       reg,
		Version:        "test",
		Logger:         log.New(io.Discard, "", 0),
		ModelOverrides: store,
		Usage:          NewUsageStore(8),
	})

	body := `{"model":"m1","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`
	if rec := chat(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := alphaBase.seen.Options["reasoning_effort"]; got != "max" {
		t.Fatalf("alpha reasoning_effort = %v, want max", got)
	}
	if got := betaBase.seen.Options["reasoning_effort"]; got != "xhigh" {
		t.Fatalf("beta reasoning_effort = %v, want the original xhigh", got)
	}
}
