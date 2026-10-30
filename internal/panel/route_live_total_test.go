package panel

import (
	"testing"
	"time"

	"client2api/internal/gateway"
)

// TestRouteLiveTotalTracesCountsTheWindowNotTheDraw pins the KPI fix: the
// counter must report how many traces the window holds, counted before the
// draw limit cuts the list. Before, the page showed len(Traces), which is
// capped at the request's limit — on a busy day the KPI sat at exactly 60 no
// matter how many requests there were.
func TestRouteLiveTotalTracesCountsTheWindowNotTheDraw(t *testing.T) {
	now := time.Now()
	// 80 one-lane traces inside the window; the view will ask for 60.
	var recs []gateway.UsageRecord
	for i := 0; i < 80; i++ {
		at := now.Add(-time.Duration(i) * time.Minute)
		recs = append(recs, gateway.UsageRecord{
			At:        at,
			StartedAt: at,
			Client:    "trae",
			Account:   "a1",
			Model:     "m",
			Candidate: 1,
		})
	}
	// And one old trace outside the window: it must not be counted.
	recs = append(recs, gateway.UsageRecord{
		At:        now.Add(-48 * time.Hour),
		StartedAt: now.Add(-48 * time.Hour),
		Client:    "trae",
		Account:   "a1",
		Model:     "m",
		Candidate: 1,
	})

	rep := buildRouteLiveReport(recs, 60, 24, now)
	if rep.TotalTraces != 80 {
		t.Errorf("TotalTraces = %d, want 80 (the window holds 80; the 48h-old record must not count)", rep.TotalTraces)
	}
	if len(rep.Traces) != 60 {
		t.Errorf("Traces = %d, want 60 (the draw limit)", len(rep.Traces))
	}
	if rep.Rerouted != 0 {
		t.Errorf("Rerouted = %d, want 0: every trace is single-lane", rep.Rerouted)
	}
}

// TestRouteLiveReroutedCountsMultiLaneTraces pins the rerouted counter: a
// trace counts once when the router offered more than one lane, regardless of
// how many rows a retried lane accumulated in place.
func TestRouteLiveTotalTracesReroutedCountsMultiLane(t *testing.T) {
	now := time.Now()
	base := func(at time.Time, cand int, failed bool) gateway.UsageRecord {
		return gateway.UsageRecord{
			At: at, StartedAt: at, Client: "trae", Account: "a", Model: "m",
			Candidate: cand, Failed: failed,
		}
	}
	// One rerouted request (two lanes), one retried-in-place request (same
	// lane twice, still single-lane), one plain request.
	recs := []gateway.UsageRecord{
		base(now.Add(-2*time.Minute), 1, true),
		base(now.Add(-2*time.Minute), 2, false),
		base(now.Add(-1*time.Minute), 1, true), // retry in place
		base(now.Add(-1*time.Minute), 1, false),
		base(now.Add(-30*time.Second), 1, false),
	}
	rep := buildRouteLiveReport(recs, 60, 24, now)
	if rep.TotalTraces != 3 {
		t.Errorf("TotalTraces = %d, want 3 (rerouted + retried + plain)", rep.TotalTraces)
	}
	if rep.Rerouted != 1 {
		t.Errorf("Rerouted = %d, want 1: a retried-in-place lane is not a reroute", rep.Rerouted)
	}
}
