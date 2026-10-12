package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// TestFailedCandidateAppearsInRecentAttempts pins the operator-facing
// mismatch: an alert is raised for a platform failure round, but if failover
// succeeds elsewhere the recent list only showed the winning platform.  The
// failed attempt must be visible without inflating the request totals.
func TestFailedCandidateAppearsInRecentAttempts(t *testing.T) {
	alpha := &testClient{name: "alpha", chatErr: core.Fail("alpha", "a1", core.FailureRateLimited, http.StatusTooManyRequests, errors.New("quota exhausted"))}
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
	var alphaAttempt UsageRecord
	for _, row := range rows {
		if row.Client == "alpha" && row.Failed && row.Attempt {
			sawAlphaAttempt = true
			alphaAttempt = row
		}
		if row.Client == "beta" && !row.Failed {
			sawBetaSuccess = true
		}
	}
	if !sawAlphaAttempt {
		t.Fatalf("recent rows = %+v, want the failed alpha attempt", rows)
	}
	attemptJSON, err := json.Marshal(alphaAttempt)
	if err != nil {
		t.Fatalf("marshal failed attempt: %v", err)
	}
	if !strings.Contains(string(attemptJSON), `"failure_reason"`) {
		t.Fatalf("failed attempt has no failure_reason: %s", attemptJSON)
	}
	if !strings.Contains(string(attemptJSON), "quota exhausted") {
		t.Fatalf("failed attempt does not carry the upstream reason: %s", attemptJSON)
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
