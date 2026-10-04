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

// TestChatSkipsUnavailablePlatformDuringFailover pins the bare-model failover
// contract when an intermediate platform has no usable account. A local
// availability failure is not an upstream refusal: the router must continue to
// the next candidate instead of turning one broken pool into the final answer.
func TestChatSkipsUnavailablePlatformDuringFailover(t *testing.T) {
	alpha := &testClient{
		name:    "alpha",
		chatErr: core.Fail("alpha", "a1", core.FailureUpstream, http.StatusBadGateway, errors.New("boom")),
	}
	unavailable := &testClient{name: "unavailable", chatErr: core.ErrNotConfigured}
	gamma := &testClient{name: "gamma", servedBy: "g1", events: usageEvents(3, 4)}

	reg := core.NewRegistry()
	reg.Add(alpha)
	reg.Add(unavailable)
	reg.Add(gamma)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"alpha":       {Priority: 1},
		"unavailable": {Priority: 2},
		"gamma":       {Priority: 3},
	})

	usage := NewUsageStore(10)
	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    usage,
		Live: livecfg.New(livecfg.Snapshot{
			MaxRotate:         1,
			RotateBackoffBase: time.Nanosecond,
		}),
	})

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if gamma.seen == nil {
		t.Fatal("the third platform never received the request")
	}

	got := usage.Snapshot()
	if len(got) != 1 || got[0].Failed || got[0].Client != "gamma" || got[0].Account != "g1" {
		t.Fatalf("usage = %+v, want one successful gamma/g1 record", got)
	}
	if got[0].Candidate != 3 {
		t.Fatalf("candidate = %d, want 3 after skipping the unavailable platform", got[0].Candidate)
	}
}
