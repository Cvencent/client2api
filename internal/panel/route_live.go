package panel

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"client2api/internal/gateway"
)

// The routing-live view asks one question: what did the gateway decide, in
// what order, and how did each attempt end?  That is not a new measurement --
// the usage store already keeps it.  Every candidate the router tried is a
// UsageRecord, and an attempt that failed before the request moved on is
// marked Attempt with the vendor's own FailureReason.
//
// This endpoint therefore only reshapes those rows into the "one request, its
// chain of attempts" form the diagram draws.  Nothing here re-decides routing,
// invents an attempt, or estimates a step: a lane that is drawn is a record
// that exists.

// RouteLiveLane is one candidate the router offered for a request, in the
// order it was tried.
type RouteLiveLane struct {
	// Candidate is 1-based, as the router numbered it.
	Candidate int    `json:"candidate"`
	Client    string `json:"client,omitempty"`
	Account   string `json:"account,omitempty"`
	// AccountLabel is the operator-facing name, when the module could give one.
	AccountLabel string `json:"account_label,omitempty"`
	Model        string `json:"model,omitempty"`
	// Failed says this lane refused the request, so the chain moved on.
	Failed bool `json:"failed,omitempty"`
	// Reason is the vendor's or gateway's own words for a failed lane.
	Reason     string `json:"reason,omitempty"`
	LatencyMS  int64  `json:"latency_ms,omitempty"`
	HasLatency bool   `json:"has_latency,omitempty"`
	// Attempts counts the recent rows that landed on this lane for this
	// request.  It is 1 for an ordinary lane; more means the same lane was
	// retried in place (a transient read timeout, say).
	Attempts int `json:"attempts"`
}

// RouteLiveTrace is one request and everything the router did for it.
type RouteLiveTrace struct {
	// ID is stable across polls: it is the request's own start time, which
	// the store records once per inbound request.
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	StartedAt time.Time `json:"started_at,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	// Client is the inbound caller's platform name, when the row names one.
	Client string `json:"client,omitempty"`
	Model  string `json:"model,omitempty"`
	// Requested is the model string as the caller asked for it, which is what
	// an Auto/ or group request looks like before resolution.
	Requested string `json:"requested,omitempty"`
	// Lanes are the candidates in tried order.
	Lanes []RouteLiveLane `json:"lanes"`
	// Winner indexes the lane that served the request, or -1 when every lane
	// failed.
	Winner int `json:"winner"`
	// Failed is true when no lane served it.
	Failed bool `json:"failed"`
	// Reason explains a wholly failed trace.
	Reason      string `json:"reason,omitempty"`
	LatencyMS   int64  `json:"latency_ms,omitempty"`
	HasLatency  bool   `json:"has_latency,omitempty"`
	TotalTokens int    `json:"total_tokens,omitempty"`
}

// RouteLiveLaneStat is one account's standing across the window, which the
// diagram draws as how lit its station is.
type RouteLiveLaneStat struct {
	Client       string `json:"client"`
	Account      string `json:"account,omitempty"`
	AccountLabel string `json:"account_label,omitempty"`
	Requests     int    `json:"requests"`
	Failures     int    `json:"failures"`
	// Picked is how many requests this lane served (the winner, not merely a
	// candidate that was offered).
	Picked     int     `json:"picked"`
	Offered    int     `json:"offered"`
	AvgLatency float64 `json:"avg_latency_ms,omitempty"`
}

// RouteLiveReport is the view's whole payload.
type RouteLiveReport struct {
	// Traces is newest first, bounded by the request's limit.
	Traces []RouteLiveTrace `json:"traces"`
	// Lanes is every account seen in the window, busiest first.
	Lanes []RouteLiveLaneStat `json:"lanes"`
	// WindowHours is how far back the aggregation looked.
	WindowHours int `json:"window_hours"`
	// Generated lets the page show whether it is looking at live data.
	Generated time.Time `json:"generated"`
	// Rerouted counts traces where more than one lane was offered, which is
	// the number the view's own headline reports.
	Rerouted int `json:"rerouted"`
}

const (
	defaultRouteLiveTraces = 40
	maxRouteLiveTraces     = 200
	defaultRouteLiveHours  = 24
)

// handleRouteLive serves the routing view: recent requests, each with the
// chain of candidates the router tried, plus a per-lane standing.
func (p *panel) handleRouteLive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}

	limit := clampQueryInt(r.URL.Query().Get("limit"), defaultRouteLiveTraces, 1, maxRouteLiveTraces)
	window := clampQueryInt(r.URL.Query().Get("window"), defaultRouteLiveHours, 1, maxUsageWindowHours)

	if p.opts.Usage == nil {
		writeJSON(w, http.StatusOK, RouteLiveReport{
			Traces: []RouteLiveTrace{}, Lanes: []RouteLiveLaneStat{},
			WindowHours: window, Generated: time.Now().UTC(),
		})
		return
	}

	recs := p.opts.Usage.Snapshot()
	report := buildRouteLiveReport(recs, limit, window, time.Now())

	if p.opts.Registry != nil {
		ctx, cancel := p.ctx(r, usageNickTimeout)
		defer cancel()
		nicks := p.accountNicks(ctx)
		labelLanes(report.Lanes, nicks)
		for i := range report.Traces {
			for j := range report.Traces[i].Lanes {
				lane := &report.Traces[i].Lanes[j]
				if lane.Account == "" {
					continue
				}
				lane.AccountLabel = nicks[lane.Account]
			}
		}
	}
	writeJSON(w, http.StatusOK, report)
}

// clampQueryInt reads one bounded integer query parameter.  An unparseable
// value falls back to the default rather than failing the page: this endpoint
// is polled on a timer and a bad hand-written URL should not blank the view.
func clampQueryInt(raw string, def, lo, hi int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// buildRouteLiveReport folds the usage ring into traces and lane standings.
// It is pure so the shape can be tested without a gateway.
func buildRouteLiveReport(recs []gateway.UsageRecord, limit, windowHours int, now time.Time) RouteLiveReport {
	report := RouteLiveReport{
		Traces:      []RouteLiveTrace{},
		Lanes:       []RouteLiveLaneStat{},
		WindowHours: windowHours,
		Generated:   now.UTC(),
	}
	if len(recs) == 0 {
		return report
	}

	cutoff := now.Add(-time.Duration(windowHours) * time.Hour)

	// Group by the request's start time.  The store records StartedAt once for
	// the inbound request and reuses it for every attempt row, so it is the
	// only field that ties a chain together without guessing.  Rows that never
	// carried one (an old record, or a caller that sent no timing) fall back to
	// At, which keeps them visible as one-row traces.
	type group struct {
		trace RouteLiveTrace
		lanes map[int]*RouteLiveLane
		order []int
	}
	groups := map[string]*group{}
	var ids []string

	for _, rec := range recs {
		if !rec.At.IsZero() && rec.At.Before(cutoff) {
			continue
		}
		key := traceKey(rec)
		g := groups[key]
		if g == nil {
			g = &group{
				trace: RouteLiveTrace{
					ID:        key,
					At:        rec.At,
					StartedAt: rec.StartedAt,
					SessionID: rec.SessionID,
					Client:    rec.Client,
					Model:     rec.Model,
					Winner:    -1,
				},
				lanes: map[int]*RouteLiveLane{},
			}
			groups[key] = g
			ids = append(ids, key)
		}
		// The caller's model string is the same on every row of one request,
		// so the first non-empty one is the answer.
		if g.trace.Model == "" {
			g.trace.Model = rec.Model
		}
		if g.trace.Client == "" {
			g.trace.Client = rec.Client
		}

		cand := rec.Candidate
		if cand <= 0 {
			cand = 1
		}
		lane := g.lanes[cand]
		if lane == nil {
			lane = &RouteLiveLane{
				Candidate:  cand,
				Client:     rec.Client,
				Account:    rec.Account,
				Model:      rec.Model,
				LatencyMS:  rec.LatencyMs,
				HasLatency: rec.HasLatency,
				Attempts:   0,
			}
			g.lanes[cand] = lane
			g.order = append(g.order, cand)
		}
		lane.Attempts++
		// A lane is failed only if its last row says so: a retry in place that
		// eventually served the request must not be drawn as a refusal.
		lane.Failed = rec.Failed
		if rec.Failed {
			lane.Reason = rec.FailureReason
		} else {
			lane.Reason = ""
		}
		if rec.HasLatency {
			lane.LatencyMS = rec.LatencyMs
			lane.HasLatency = true
		}
		if rec.FailureReason != "" && rec.Failed {
			g.trace.Reason = rec.FailureReason
		}
	}

	sort.Slice(ids, func(i, j int) bool {
		a, b := groups[ids[i]].trace, groups[ids[j]].trace
		if !a.At.Equal(b.At) {
			return a.At.After(b.At)
		}
		return ids[i] > ids[j]
	})
	if len(ids) > limit {
		ids = ids[:limit]
	}

	laneStats := map[string]*RouteLiveLaneStat{}
	for _, id := range ids {
		g := groups[id]
		sort.Ints(g.order)
		trace := g.trace
		trace.Lanes = make([]RouteLiveLane, 0, len(g.order))
		winner := -1
		for i, cand := range g.order {
			lane := *g.lanes[cand]
			trace.Lanes = append(trace.Lanes, lane)

			key := laneStatKey(lane.Client, lane.Account)
			st := laneStats[key]
			if st == nil {
				st = &RouteLiveLaneStat{Client: lane.Client, Account: lane.Account}
				laneStats[key] = st
			}
			st.Offered++
			if lane.Failed {
				st.Failures++
			}
			if lane.HasLatency {
				st.AvgLatency += float64(lane.LatencyMS)
				st.Requests++
			}
			if !lane.Failed && winner < 0 {
				winner = i
			}
		}
		trace.Winner = winner
		trace.Failed = winner < 0
		if winner >= 0 {
			w := trace.Lanes[winner]
			trace.LatencyMS = w.LatencyMS
			trace.HasLatency = w.HasLatency
			trace.Reason = ""
			key := laneStatKey(w.Client, w.Account)
			if st := laneStats[key]; st != nil {
				st.Picked++
			}
		}
		if len(trace.Lanes) > 1 {
			report.Rerouted++
		}
		report.Traces = append(report.Traces, trace)
	}

	for _, st := range laneStats {
		if st.Requests > 0 {
			st.AvgLatency /= float64(st.Requests)
		}
		report.Lanes = append(report.Lanes, *st)
	}
	sort.Slice(report.Lanes, func(i, j int) bool {
		a, b := report.Lanes[i], report.Lanes[j]
		if a.Picked != b.Picked {
			return a.Picked > b.Picked
		}
		if a.Offered != b.Offered {
			return a.Offered > b.Offered
		}
		if a.Client != b.Client {
			return a.Client < b.Client
		}
		return a.Account < b.Account
	})
	return report
}

// traceKey identifies the inbound request a row belongs to.
//
// StartedAt is the request's own start, recorded once and copied to each
// attempt row, so it is the honest tie.  A row without it falls back to At:
// that cannot merge two real attempts, and it keeps the row visible rather
// than dropping a request the store did record.
func traceKey(rec gateway.UsageRecord) string {
	if !rec.StartedAt.IsZero() {
		return rec.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	return rec.At.UTC().Format(time.RFC3339Nano)
}

func laneStatKey(client, account string) string {
	return strings.ToLower(client) + "\x00" + account
}

// labelLanes fills the operator-facing account names the panel already resolves
// for the usage view, so a lane reads "z.ai panel login" rather than an id.
func labelLanes(lanes []RouteLiveLaneStat, nicks map[string]string) {
	for i := range lanes {
		if lanes[i].Account == "" {
			continue
		}
		lanes[i].AccountLabel = nicks[lanes[i].Account]
	}
}
