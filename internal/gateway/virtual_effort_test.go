package gateway

import (
	"context"
	"io"
	"log"
	"net/http"
	"reflect"
	"testing"

	"client2api/internal/core"
	"client2api/internal/modelmeta"
)

type virtualEffortClient struct {
	*testClient
	efforts       []string
	defaultEffort string
}

func (c *virtualEffortClient) ModelEfforts(context.Context, string) ([]string, string, bool) {
	return append([]string(nil), c.efforts...), c.defaultEffort, len(c.efforts) > 0
}

func regWith(clients ...core.Client) *core.Registry {
	reg := core.NewRegistry()
	for _, c := range clients {
		reg.Add(c)
	}
	return reg
}

func TestVirtualAutoPublishesUnionEffortsAndFirstRoutingDefault(t *testing.T) {
	alpha := &virtualEffortClient{
		testClient:    &testClient{name: "alpha", catalogue: []core.Model{{ID: "shared-model"}}},
		efforts:       []string{"low", "high"},
		defaultEffort: "high",
	}
	beta := &virtualEffortClient{
		testClient:    &testClient{name: "beta", catalogue: []core.Model{{ID: "shared-model"}}},
		efforts:       []string{"medium", "max"},
		defaultEffort: "medium",
	}
	reg := regWith(alpha, beta)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"alpha": {Priority: 1},
		"beta":  {Priority: 2},
	})
	srv := NewServer(Options{Registry: reg, Version: "test", Logger: log.New(io.Discard, "", 0)})

	entry := listModels(t, srv)["Auto/shared-model"]
	if entry == nil {
		t.Fatal("Auto/shared-model missing from /v1/models")
	}
	extra, _ := entry["extra"].(map[string]any)
	if extra == nil {
		t.Fatal("Auto entry has no extra metadata")
	}
	if got := extra["supported_efforts"]; !reflect.DeepEqual(got, []any{"low", "high", "medium", "max"}) {
		t.Fatalf("Auto supported_efforts = %#v, want union in routing order", got)
	}
	if got := extra["default_effort"]; got != "high" {
		t.Fatalf("Auto default_effort = %#v, want first routing candidate's high", got)
	}
}

func TestVirtualAutoEffortOverrideBeatsAggregatedFallback(t *testing.T) {
	base := &virtualEffortClient{
		testClient:    &testClient{name: "alpha", catalogue: []core.Model{{ID: "shared-model"}}},
		efforts:       []string{"low", "high"},
		defaultEffort: "high",
	}
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetEfforts(core.VirtualModelClient, "Auto/shared-model", []string{"high", "max"}, "max"); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Options{
		Registry:       regWith(base),
		Version:        "test",
		Logger:         log.New(io.Discard, "", 0),
		ModelOverrides: store,
	})

	entry := listModels(t, srv)["Auto/shared-model"]
	extra, _ := entry["extra"].(map[string]any)
	if got := extra["supported_efforts"]; !reflect.DeepEqual(got, []any{"high", "max"}) {
		t.Fatalf("manual Auto supported_efforts = %#v, want [high max]", got)
	}
	if got := extra["default_effort"]; got != "max" {
		t.Fatalf("manual Auto default_effort = %#v, want max", got)
	}
}

func TestVirtualGroupEffortFallbackAndOverride(t *testing.T) {
	alpha := &virtualEffortClient{
		testClient:    &testClient{name: "alpha", catalogue: []core.Model{{ID: "alpha-model"}}},
		efforts:       []string{"low", "high"},
		defaultEffort: "low",
	}
	beta := &virtualEffortClient{
		testClient:    &testClient{name: "beta", catalogue: []core.Model{{ID: "beta-model"}}},
		efforts:       []string{"medium", "max"},
		defaultEffort: "medium",
	}
	reg := regWith(alpha, beta)
	reg.SetModelGroups(map[string]core.ModelGroup{
		"shared": {Members: []core.ModelGroupMember{
			{Client: "alpha", Model: "alpha-model"},
			{Client: "beta", Model: "beta-model"},
		}},
	})
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Options{Registry: reg, Version: "test", Logger: log.New(io.Discard, "", 0), ModelOverrides: store})

	entry := listModels(t, srv)["shared"]
	extra, _ := entry["extra"].(map[string]any)
	if got := extra["supported_efforts"]; !reflect.DeepEqual(got, []any{"low", "high", "medium", "max"}) {
		t.Fatalf("group supported_efforts = %#v, want [low high medium max]", got)
	}
	if got := extra["default_effort"]; got != "low" {
		t.Fatalf("group default_effort = %#v, want first member default low", got)
	}

	if err := store.SetEfforts(core.VirtualModelClient, "shared", []string{"high", "max"}, "max"); err != nil {
		t.Fatal(err)
	}
	entry = listModels(t, srv)["shared"]
	extra, _ = entry["extra"].(map[string]any)
	if got := extra["supported_efforts"]; !reflect.DeepEqual(got, []any{"high", "max"}) {
		t.Fatalf("manual group supported_efforts = %#v, want [high max]", got)
	}
	if got := extra["default_effort"]; got != "max" {
		t.Fatalf("manual group default_effort = %#v, want max", got)
	}
}

func TestVirtualDefaultEffortIsInjectedWhenClientOmitsIt(t *testing.T) {
	alphaBase := &testClient{name: "alpha", catalogue: []core.Model{{ID: "shared-model"}}}
	alpha := &virtualEffortClient{testClient: alphaBase, efforts: []string{"low", "high"}, defaultEffort: "high"}
	betaBase := &testClient{name: "beta", catalogue: []core.Model{{ID: "shared-model"}}}
	beta := &virtualEffortClient{testClient: betaBase, efforts: []string{"low", "high"}, defaultEffort: "low"}
	reg := regWith(alpha, beta)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{"alpha": {Priority: 1}, "beta": {Priority: 2}})
	srv := NewServer(Options{Registry: reg, Version: "test", Logger: log.New(io.Discard, "", 0)})

	rec := chat(t, srv, `{"model":"Auto/shared-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if alphaBase.seen == nil {
		t.Fatal("the highest-priority candidate was not called")
	}
	if got := alphaBase.seen.Options["reasoning_effort"]; got != "high" {
		t.Fatalf("injected Auto default effort = %v, want high", got)
	}
	if betaBase.seen != nil {
		t.Fatal("a candidate without an explicit failure was reordered or skipped")
	}
}

func TestExplicitEffortBeatsVirtualDefaultAndUnsupportedCandidateDowngradesInOrder(t *testing.T) {
	alphaBase := &testClient{name: "alpha", catalogue: []core.Model{{ID: "shared-model"}}}
	alpha := &virtualEffortClient{testClient: alphaBase, efforts: []string{"low", "high"}, defaultEffort: "low"}
	betaBase := &testClient{name: "beta", catalogue: []core.Model{{ID: "shared-model"}}}
	beta := &virtualEffortClient{testClient: betaBase, efforts: []string{"low", "high", "max"}, defaultEffort: "low"}
	reg := regWith(alpha, beta)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{"alpha": {Priority: 1}, "beta": {Priority: 2}})
	srv := NewServer(Options{Registry: reg, Version: "test", Logger: log.New(io.Discard, "", 0)})

	rec := chat(t, srv, `{"model":"Auto/shared-model","reasoning_effort":"max","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if alphaBase.seen == nil {
		t.Fatal("alpha was not tried first")
	}
	if got := alphaBase.seen.Options["reasoning_effort"]; got != "high" {
		t.Fatalf("alpha effort = %v, want the supported cap high without reordering", got)
	}
	if betaBase.seen != nil {
		t.Fatal("beta was tried even though alpha could serve after cap alignment")
	}
}

func TestVirtualCapAlignmentOffOverridesTheCandidateDefault(t *testing.T) {
	alpha := &virtualEffortClient{
		testClient:    &testClient{name: "alpha", catalogue: []core.Model{{ID: "shared-model"}}},
		efforts:       []string{"low", "high", "max"},
		defaultEffort: "high",
	}
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetElevateTop(core.VirtualModelClient, "Auto/shared-model", false); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Options{
		Registry:       regWith(alpha),
		Version:        "test",
		Logger:         log.New(io.Discard, "", 0),
		ModelOverrides: store,
	})

	rec := chat(t, srv, `{"model":"Auto/shared-model","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := alpha.seen.Options["reasoning_effort"]; got != "xhigh" {
		t.Fatalf("sent effort = %v, want xhigh after the virtual row disabled cap alignment", got)
	}
}

func TestVirtualCapAlignmentOnOverridesTheCandidateChoice(t *testing.T) {
	alpha := &virtualEffortClient{
		testClient:    &testClient{name: "alpha", catalogue: []core.Model{{ID: "shared-model"}}},
		efforts:       []string{"low", "high", "max"},
		defaultEffort: "high",
	}
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetElevateTop("alpha", "shared-model", false); err != nil {
		t.Fatal(err)
	}
	if err := store.SetElevateTop(core.VirtualModelClient, "Auto/shared-model", true); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Options{
		Registry:       regWith(alpha),
		Version:        "test",
		Logger:         log.New(io.Discard, "", 0),
		ModelOverrides: store,
	})

	rec := chat(t, srv, `{"model":"Auto/shared-model","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := alpha.seen.Options["reasoning_effort"]; got != "max" {
		t.Fatalf("sent effort = %v, want max after the virtual row enabled cap alignment", got)
	}
}

func TestVirtualExplicitEffortMappingsOverrideCandidateRules(t *testing.T) {
	alpha := &virtualEffortClient{
		testClient:    &testClient{name: "alpha", catalogue: []core.Model{{ID: "shared-model"}}},
		efforts:       []string{"low", "high", "max"},
		defaultEffort: "high",
	}
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetEffortMappings("alpha", "shared-model", []modelmeta.EffortMapping{{From: "high", To: "low"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetEffortMappings(core.VirtualModelClient, "Auto/shared-model", []modelmeta.EffortMapping{{From: "high", To: "max"}}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Options{
		Registry:       regWith(alpha),
		Version:        "test",
		Logger:         log.New(io.Discard, "", 0),
		ModelOverrides: store,
	})

	rec := chat(t, srv, `{"model":"Auto/shared-model","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := alpha.seen.Options["reasoning_effort"]; got != "max" {
		t.Fatalf("sent effort = %v, want the virtual row's high -> max rule", got)
	}
}

func TestVirtualFallsBackToCandidateEffortMappings(t *testing.T) {
	alpha := &virtualEffortClient{
		testClient:    &testClient{name: "alpha", catalogue: []core.Model{{ID: "shared-model"}}},
		efforts:       []string{"low", "high", "max"},
		defaultEffort: "high",
	}
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetEffortMappings("alpha", "shared-model", []modelmeta.EffortMapping{{From: "high", To: "max"}}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Options{
		Registry:       regWith(alpha),
		Version:        "test",
		Logger:         log.New(io.Discard, "", 0),
		ModelOverrides: store,
	})

	rec := chat(t, srv, `{"model":"Auto/shared-model","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := alpha.seen.Options["reasoning_effort"]; got != "max" {
		t.Fatalf("sent effort = %v, want the candidate's high -> max rule", got)
	}
}

func TestVirtualExplicitEffortMappingIsFinalForGroupMembers(t *testing.T) {
	alpha := &virtualEffortClient{
		testClient:    &testClient{name: "alpha", catalogue: []core.Model{{ID: "alpha-model"}}},
		efforts:       []string{"high"},
		defaultEffort: "high",
	}
	reg := regWith(alpha)
	reg.SetModelGroups(map[string]core.ModelGroup{
		"shared": {Members: []core.ModelGroupMember{{Client: "alpha", Model: "alpha-model"}}},
	})
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetEffortMappings(core.VirtualModelClient, "shared", []modelmeta.EffortMapping{{From: "max", To: "high"}}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Options{
		Registry:       reg,
		Version:        "test",
		Logger:         log.New(io.Discard, "", 0),
		ModelOverrides: store,
	})

	rec := chat(t, srv, `{"model":"shared","reasoning_effort":"max","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := alpha.testClient.seen.Options["reasoning_effort"]; got != "high" {
		t.Fatalf("sent effort = %v, want the group's explicit max -> high mapping", got)
	}
}
