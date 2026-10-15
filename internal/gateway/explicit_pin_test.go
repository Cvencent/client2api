package gateway

import (
	"errors"
	"io"
	"log"
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/livecfg"
)

// Writing client/model is a statement about which platform to use, so the
// gateway must answer from that platform even when the same model id also sits
// in a model group.  This is the operator-visible symptom: an explicit
// "loomy/..." silently arriving from another platform.
func TestExplicitPlatformPinSurvivesAModelGroup(t *testing.T) {
	loomy := &testClient{
		name:      "loomy",
		catalogue: []core.Model{{ID: "deepseek-v4.1-flash"}},
		servedBy:  "loomy-1",
		events:    usageEvents(1, 1),
	}
	other := &testClient{
		name:      "other",
		catalogue: []core.Model{{ID: "deepseek-v4.1-flash"}},
		servedBy:  "other-1",
		events:    usageEvents(1, 1),
	}

	reg := core.NewRegistry()
	reg.Add(loomy)
	reg.Add(other)
	// other outranks loomy, so only a real pin keeps the request on loomy.
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"loomy": {Priority: 5},
		"other": {Priority: 1},
	})
	reg.SetModelGroups(map[string]core.ModelGroup{
		"deepseek-flash": {Members: []core.ModelGroupMember{
			{Client: "loomy", Model: "deepseek-v4.1-flash"},
			{Client: "other", Model: "deepseek-v4.1-flash"},
		}},
	})

	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    NewUsageStore(10),
	})

	rec := chat(t, srv, `{"model":"loomy/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if loomy.seen == nil {
		t.Fatal("the explicitly named platform never received the request")
	}
	if other.seen != nil {
		t.Fatalf("the request was served by another platform: %+v", other.seen)
	}

	// The bare group name still follows the operator's priority order.  A
	// different first turn, so the earlier conversation's stickiness does not
	// decide this one.
	rec = chat(t, srv, `{"model":"deepseek-flash","messages":[{"role":"user","content":"another question"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("group status = %d, body %s", rec.Code, rec.Body.String())
	}
	if other.seen == nil {
		t.Fatal("the higher-priority group member did not receive the group request")
	}
}

// The pin is about the platform, not about succeeding at any cost: when the
// named platform is genuinely exhausted, the caller is told so instead of being
// quietly served by a platform it did not ask for.
func TestExplicitPlatformPinReportsItsOwnFailure(t *testing.T) {
	loomy := &testClient{
		name:      "loomy",
		catalogue: []core.Model{{ID: "deepseek-v4.1-flash"}},
		chatErr:   core.Fail("loomy", "loomy-1", core.FailureQuota, http.StatusBadGateway, errors.New("免费额度已用完，请升级套餐")),
	}
	other := &testClient{
		name:      "other",
		catalogue: []core.Model{{ID: "deepseek-v4.1-flash"}},
		servedBy:  "other-1",
		events:    usageEvents(1, 1),
	}

	reg := core.NewRegistry()
	reg.Add(loomy)
	reg.Add(other)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"loomy": {Priority: 5},
		"other": {Priority: 1},
	})
	reg.SetModelGroups(map[string]core.ModelGroup{
		"deepseek-flash": {Members: []core.ModelGroupMember{
			{Client: "loomy", Model: "deepseek-v4.1-flash"},
			{Client: "other", Model: "deepseek-v4.1-flash"},
		}},
	})

	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    NewUsageStore(10),
		Live: livecfg.New(livecfg.Snapshot{
			MaxRotate:         1,
			RotateBackoffBase: time.Nanosecond,
		}),
	})

	rec := chat(t, srv, `{"model":"loomy/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code == http.StatusOK {
		t.Fatal("an exhausted explicitly named platform was answered by another platform")
	}
	if other.seen != nil {
		t.Fatalf("another platform served a request pinned to loomy: %+v", other.seen)
	}
}
