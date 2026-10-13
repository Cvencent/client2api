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

func TestElevateTopDefaultsOnWithoutAnEmbeddedTableRow(t *testing.T) {
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
	if got := base.seen.Options["reasoning_effort"]; got != "max" {
		t.Fatalf("module reasoning_effort = %v, want max from the all-model default", got)
	}
	if !base.seen.EffortFinal {
		t.Fatal("the all-model promotion was not marked final")
	}
}

// TestElevateTopEmbeddedDefaultNeedsNoOverrideStore pins the shipped default:
// workbuddy's V4.1 Flash row opts into cap alignment inside the binary, so the
// promotion happens even when the panel store was never wired or never touched.
func TestElevateTopEmbeddedDefaultNeedsNoOverrideStore(t *testing.T) {
	base := &testClient{name: "workbuddy", catalogue: []core.Model{{ID: "cn:deepseek-v4.1-flash"}}}
	srv := newEffortPolicyServer(t, base, nil)

	body := `{"model":"cn:deepseek-v4.1-flash","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`
	if rec := chat(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := base.seen.Options["reasoning_effort"]; got != "max" {
		t.Fatalf("module reasoning_effort = %v, want the embedded default to promote xhigh to max", got)
	}
	if !base.seen.EffortFinal {
		t.Fatal("the embedded promotion was not marked final")
	}
}

// OpenRouter's live V4.1 Flash row publishes low/high/max, so the embedded
// default must promote xhigh even when the panel store was never wired.
func TestElevateTopEmbeddedDefaultOpenRouter(t *testing.T) {
	base := &testClient{name: "openrouter", catalogue: []core.Model{{ID: "deepseek/deepseek-v4.1-flash"}}}
	srv := newEffortPolicyServer(t, base, nil)

	body := `{"model":"deepseek/deepseek-v4.1-flash","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`
	if rec := chat(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := base.seen.Options["reasoning_effort"]; got != "max" {
		t.Fatalf("module reasoning_effort = %v, want the embedded default to promote xhigh to max", got)
	}
	if !base.seen.EffortFinal {
		t.Fatal("the embedded promotion was not marked final")
	}
}

// TestElevateTopEmbeddedDefaultNeverInventsMax guards the realm split: the
// international V4.1 Flash row accepts only high, so the on-by-default policy
// must leave the caller's xhigh alone rather than sending a value that 400s.
func TestElevateTopEmbeddedDefaultNeverInventsMax(t *testing.T) {
	base := &testClient{name: "workbuddy", catalogue: []core.Model{{ID: "global:deepseek-v4.1-flash"}}}
	srv := newEffortPolicyServer(t, base, nil)

	body := `{"model":"global:deepseek-v4.1-flash","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`
	if rec := chat(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := base.seen.Options["reasoning_effort"]; got != "xhigh" {
		t.Fatalf("module reasoning_effort = %v, want xhigh when the realm publishes no higher tier", got)
	}
}

// TestElevateTopExplicitOffBeatsEmbeddedDefault and restore: an operator who
// unchecks the box must keep xhigh, and "restore default" must bring the
// embedded promotion back instead of pinning a permanent off.
func TestElevateTopExplicitOffBeatsEmbeddedDefault(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetElevateTop("cline", "cline-free/deepseek-v4.1-flash", false); err != nil {
		t.Fatal(err)
	}
	base := &testClient{name: "cline", catalogue: []core.Model{{ID: "cline-free/deepseek-v4.1-flash"}}}
	srv := newEffortPolicyServer(t, base, store)

	body := `{"model":"cline-free/deepseek-v4.1-flash","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`
	if rec := chat(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := base.seen.Options["reasoning_effort"]; got != "xhigh" {
		t.Fatalf("module reasoning_effort = %v, want xhigh after an explicit off", got)
	}

	if err := store.ClearElevateTop("cline", "cline-free/deepseek-v4.1-flash"); err != nil {
		t.Fatal(err)
	}
	if rec := chat(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("restore status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := base.seen.Options["reasoning_effort"]; got != "max" {
		t.Fatalf("module reasoning_effort = %v, want max after restore default", got)
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
