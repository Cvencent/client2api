package gateway

import (
	"errors"
	"io"
	"log"
	"net/http"
	"testing"

	"client2api/internal/core"
)

// TestFailedCandidateAppearsInRecentAttempts pins the operator-facing
// mismatch: an alert is raised for a platform failure round, but if failover
// succeeds elsewhere the recent list only showed the winning platform.  The
// failed attempt must be visible without inflating the request totals.
func TestFailedCandidateAppearsInRecentAttempts(t *testing.T) {
	alpha := &testClient{name: "alpha", chatErr: core.Fail("alpha", "a1", core.FailureUpstream, http.StatusBadGateway, errors.New("boom"))}
	beta := &testClient{name: "beta", events: usageEvents(1, 1)}
	reg := core.NewRegistry()
	reg.Add(alpha)
	reg.Add(beta)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"alpha": {Priority: 1},
		"beta":  {Priority: 2},
	})
	usage := NewUsageStore(20)
	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    usage,
	})

	rec := chat(t, srv, `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	rows := usage.Snapshot()
	var sawAlphaAttempt, sawBetaSuccess bool
	for _, row := range rows {
		if row.Client == "alpha" && row.Failed && row.Attempt {
			sawAlphaAttempt = true
		}
		if row.Client == "beta" && !row.Failed {
			sawBetaSuccess = true
		}
	}
	if !sawAlphaAttempt {
		t.Fatalf("recent rows = %+v, want the failed alpha attempt", rows)
	}
	if !sawBetaSuccess {
		t.Fatalf("recent rows = %+v, want the successful beta record", rows)
	}
	if got := usage.Total(); got != 1 {
		t.Fatalf("Total = %d, want 1: an intermediate attempt must not inflate request totals", got)
	}
	report := usage.UsageReport(0)
	if report.Totals.Requests != 1 || report.Totals.Failures != 0 {
		t.Fatalf("totals = %+v, want one request and zero final failures", report.Totals)
	}
}
