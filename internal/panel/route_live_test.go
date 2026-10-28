package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/gateway"
)

// The routing view draws what the gateway recorded, so its two facts have to
// be right: which candidates one request tried, in what order, and which of
// them served it.  These tests drive the pure fold directly, because the bug
// they guard against is a fold that merges two requests into one trace or
// reports a retried lane as a refusal.

func recAt(start time.Time, cand int, failed bool, reason string) gateway.UsageRecord {
	return gateway.UsageRecord{
		At:            start.Add(2 * time.Second),
		StartedAt:     start,
		Client:        "loomy",
		Account:       "acc-" + reason + string(rune('0'+cand)),
		Model:         "deepseek-v4.1-flash",
		Candidate:     cand,
		Failed:        failed,
		FailureReason: reason,
		Attempt:       failed,
		LatencyMs:     1200,
		HasLatency:    true,
	}
}

func TestRouteLiveFoldsAttemptsIntoOneTrace(t *testing.T) {
	now := time.Now()
	start := now.Add(-time.Minute)

	recs := []gateway.UsageRecord{
		// candidate 1 refused, candidate 2 served: one request, two lanes.
		recAt(start, 1, true, "429 limited"),
		recAt(start, 2, false, ""),
		// A different request, one lane.
		recAt(start.Add(-time.Hour), 1, false, ""),
	}

	report := buildRouteLiveReport(recs, 40, 24, now)
	if len(report.Traces) != 2 {
		t.Fatalf("traces = %d, want 2 (each request is one trace)", len(report.Traces))
	}

	newest := report.Traces[0]
	if len(newest.Lanes) != 2 {
		t.Fatalf("lanes = %d, want the two candidates of one request", len(newest.Lanes))
	}
	if newest.Lanes[0].Candidate != 1 || !newest.Lanes[0].Failed {
		t.Errorf("lane 1 = %+v, want the refused first candidate", newest.Lanes[0])
	}
	if newest.Lanes[0].Reason != "429 limited" {
		t.Errorf("lane 1 reason = %q, want the vendor's own words", newest.Lanes[0].Reason)
	}
	if newest.Lanes[1].Candidate != 2 || newest.Lanes[1].Failed {
		t.Errorf("lane 2 = %+v, want the candidate that served", newest.Lanes[1])
	}
	if newest.Winner != 1 {
		t.Errorf("winner = %d, want lane index 1", newest.Winner)
	}
	if newest.Failed {
		t.Error("a request with a serving lane must not be reported as failed")
	}
	if report.Rerouted != 1 {
		t.Errorf("rerouted = %d, want the one request that changed lanes", report.Rerouted)
	}
}

func TestRouteLiveReportsAWhollyFailedTrace(t *testing.T) {
	now := time.Now()
	start := now.Add(-time.Minute)

	recs := []gateway.UsageRecord{
		recAt(start, 1, true, "402 balance"),
		recAt(start, 2, true, "403 invalid"),
	}

	report := buildRouteLiveReport(recs, 40, 24, now)
	if len(report.Traces) != 1 {
		t.Fatalf("traces = %d, want 1", len(report.Traces))
	}
	tr := report.Traces[0]
	if !tr.Failed {
		t.Error("a trace whose every lane refused must be reported as failed")
	}
	if tr.Winner != -1 {
		t.Errorf("winner = %d, want -1 when nothing served", tr.Winner)
	}
	if tr.Reason == "" {
		t.Error("a failed trace must carry a reason for the view to show")
	}
}

func TestRouteLiveDoesNotCountARetriedLaneAsRefused(t *testing.T) {
	// One lane can be retried in place: a transient read timeout, then the
	// same account serving.  The last row is what the lane is, so the trace
	// must not be drawn as a refusal that never happened.
	now := time.Now()
	start := now.Add(-time.Minute)

	recs := []gateway.UsageRecord{
		recAt(start, 1, true, "read timeout"),
		recAt(start, 1, false, ""),
	}

	report := buildRouteLiveReport(recs, 40, 24, now)
	tr := report.Traces[0]
	if len(tr.Lanes) != 1 {
		t.Fatalf("lanes = %d, want the retried lane folded into one", len(tr.Lanes))
	}
	if tr.Lanes[0].Failed {
		t.Error("a lane that eventually served must not be drawn as failed")
	}
	if tr.Lanes[0].Attempts != 2 {
		t.Errorf("attempts = %d, want both rows counted", tr.Lanes[0].Attempts)
	}
	if tr.Failed || tr.Winner != 0 {
		t.Errorf("trace = %+v, want it served by its only lane", tr)
	}
}

func TestRouteLiveDropsRowsOutsideTheWindow(t *testing.T) {
	now := time.Now()
	old := now.Add(-48 * time.Hour)

	recs := []gateway.UsageRecord{recAt(old, 1, false, "")}
	report := buildRouteLiveReport(recs, 40, 24, now)
	if len(report.Traces) != 0 {
		t.Fatalf("traces = %d, want the out-of-window row dropped", len(report.Traces))
	}
}

func TestRouteLiveLaneStandingCountsPicksAndFailures(t *testing.T) {
	now := time.Now()
	base := now.Add(-time.Minute)

	var recs []gateway.UsageRecord
	// Two requests served by acc-good, one request where acc-bad refused first.
	for i := 0; i < 2; i++ {
		start := base.Add(time.Duration(-i) * time.Minute)
		recs = append(recs, gateway.UsageRecord{
			At: start.Add(time.Second), StartedAt: start, Client: "loomy",
			Account: "acc-good", Candidate: 1, LatencyMs: 1000, HasLatency: true,
		})
	}
	start := base.Add(-10 * time.Minute)
	recs = append(recs,
		gateway.UsageRecord{
			At: start.Add(time.Second), StartedAt: start, Client: "loomy",
			Account: "acc-bad", Candidate: 1, Failed: true, FailureReason: "429",
		},
		gateway.UsageRecord{
			At: start.Add(2 * time.Second), StartedAt: start, Client: "loomy",
			Account: "acc-good", Candidate: 2, LatencyMs: 2000, HasLatency: true,
		},
	)

	report := buildRouteLiveReport(recs, 40, 24, now)
	byAccount := map[string]RouteLiveLaneStat{}
	for _, st := range report.Lanes {
		byAccount[st.Account] = st
	}
	good, ok := byAccount["acc-good"]
	if !ok {
		t.Fatal("acc-good has no standing row")
	}
	if good.Picked != 3 {
		t.Errorf("acc-good picked = %d, want 3", good.Picked)
	}
	if good.Offered != 3 {
		t.Errorf("acc-good offered = %d, want 3", good.Offered)
	}
	bad, ok := byAccount["acc-bad"]
	if !ok {
		t.Fatal("acc-bad has no standing row")
	}
	if bad.Failures != 1 || bad.Picked != 0 {
		t.Errorf("acc-bad = %+v, want one failure and no pick", bad)
	}
	// Busiest first, so the account that actually served leads.
	if report.Lanes[0].Account != "acc-good" {
		t.Errorf("lanes[0] = %q, want the busiest account first", report.Lanes[0].Account)
	}
}

func TestRouteLiveEndpointRequiresGET(t *testing.T) {
	// New registers the real mux, so this pins the route as well as the
	// method guard: a handler that existed but was never mounted would answer
	// 404 here, which is the failure mode worth catching.
	srv := httptest.NewServer(New(Options{Registry: core.NewRegistry()}))
	defer srv.Close()

	res, err := http.Post(srv.URL+"/panel/api/route_live", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", res.StatusCode)
	}

	get, err := http.Get(srv.URL + "/panel/api/route_live")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer get.Body.Close()
	if get.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200 even with no usage store", get.StatusCode)
	}
}
