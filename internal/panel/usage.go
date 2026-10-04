package panel

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"client2api/internal/core"
	"client2api/internal/gateway"
)

// Usage window bounds, in hours.
const (
	defaultUsageWindowHours = 72
	maxUsageWindowHours     = 8760 // one year; beyond this the cap is meaningless
	// usageNickTimeout bounds the account-name lookup the usage view does for
	// its display labels.  A slow or wedged module must never hold the report.
	usageNickTimeout = 5 * time.Second
)

// handleUsage serves the aggregated traffic view.  All of the aggregation
// lives in the gateway's store, which owns the records; this handler only
// validates the window, resolves account labels, and renders the report.
func (p *panel) handleUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}

	window := defaultUsageWindowHours
	if raw := r.URL.Query().Get("window"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "window must be an integer number of hours")
			return
		}
		window = n
	}
	if window < 0 {
		window = defaultUsageWindowHours
	}
	if window > maxUsageWindowHours {
		window = maxUsageWindowHours
	}

	if p.opts.Usage == nil {
		writeJSON(w, http.StatusOK, emptyUsageReport(window))
		return
	}

	ctx, cancel := p.ctx(r, usageNickTimeout)
	defer cancel()
	report := p.opts.Usage.UsageSnapshot(window, p.accountNicks(ctx))
	report.Recent = p.opts.Usage.Snapshot()
	for i, j := 0, len(report.Recent)-1; i < j; i, j = i+1, j-1 {
		report.Recent[i], report.Recent[j] = report.Recent[j], report.Recent[i]
	}
	writeJSON(w, http.StatusOK, report)
}

// handleUsageSave forces the usage store to flush now.
//
// The store writes on its own timer and again on shutdown, so this is not how
// the history normally reaches the disk.  It exists for the two moments the
// timer cannot serve: an operator about to stop the process by hand, and one who
// wants a definite answer about whether the history is currently on disk.
func (p *panel) handleUsageSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if p.opts.Usage == nil {
		writeErr(w, http.StatusServiceUnavailable, "no usage store")
		return
	}
	if err := p.opts.Usage.Save(); err != nil {
		// The store keeps its own last error too, but the operator asked for
		// this write specifically, so answer with what this write hit.
		writeErr(w, http.StatusInternalServerError, core.Redact(err.Error()))
		return
	}
	out := map[string]any{"ok": true}
	if d := p.opts.Usage.Describe(); d != "" {
		out["store"] = d
	}
	// Reported only when it is set: after a successful save this is the most
	// recent previous failure, and *not* saying so would read as "all clear".
	if err := p.opts.Usage.LastError(); err != nil {
		out["last_error"] = core.Redact(err.Error())
	}
	writeJSON(w, http.StatusOK, out)
}

// emptyUsageReport is the payload served when the process has no usage store.
// Every list is non-nil so the frontend can iterate unconditionally.
func emptyUsageReport(window int) gateway.UsageReport {
	return gateway.UsageReport{
		WindowHours: window,
		ByClient:    []gateway.UsageBucket{},
		ByRealm:     []gateway.UsageBucket{},
		ByAccount:   []gateway.UsageBucket{},
		ByModel:     []gateway.UsageBucket{},
		Series:      []gateway.UsagePoint{},
		Recent:      []gateway.UsageRecord{},
		Generated:   time.Now().UTC().Format(time.RFC3339),
	}
}

// accountNicks maps an account id to the label the operator gave it, so the
// usage view can show "aws-prod-3" instead of an opaque credential id.
//
// The lookup is best effort and deliberately lossy: a module that cannot list
// its accounts contributes nothing, and the row then renders with its raw id
// rather than disappearing from the report.  Labels are redacted on the way
// out, exactly like every other place the panel shows one.
func (p *panel) accountNicks(ctx context.Context) map[string]string {
	if p.opts.Registry == nil {
		return nil
	}
	nicks := map[string]string{}
	for _, c := range p.opts.Registry.All() {
		am, ok := core.AsAccountManager(c)
		if !ok {
			continue
		}
		list, err := am.Accounts(ctx)
		if err != nil {
			continue
		}
		for _, a := range list {
			if a.ID == "" || a.Label == "" {
				continue
			}
			nicks[a.ID] = core.Redact(a.Label)
		}
	}
	return nicks
}

// accountStat is the per-account slice of the traffic report that the account
// table shows: how often the credential worked, how much it carried, and when
// it last worked.
//
// These are a separate payload key rather than fields on core.AccountRecord on
// purpose.  An AccountRecord is the module's own editable state — the thing
// "remove" and "disable" act on — while these numbers belong to the gateway's
// traffic history and change on every request.  Folding them in would make the
// account contract depend on the gateway.
type accountStat struct {
	Requests           int64    `json:"requests"`
	Failures           int64    `json:"failures"`
	TotalTokens        int64    `json:"total_tokens"`
	AvgLatencyMs       *float64 `json:"avg_latency_ms,omitempty"`
	AvgTokensPerSecond *float64 `json:"avg_tokens_per_second,omitempty"`
	LastSuccess        string   `json:"last_success,omitempty"`
}

// accountStats indexes the traffic report by account id.
//
// window 0 means "every bucket the store still holds", which is the closest
// thing this process has to the lifetime per-account counters the reference
// dashboard shows.  No account names are resolved here: the rows are keyed by
// the raw id so the caller can join them against the account list it already
// has.
func (p *panel) accountStats() map[string]accountStat {
	if p.opts.Usage == nil {
		return nil
	}
	rep := p.opts.Usage.UsageSnapshot(0, nil)
	if len(rep.ByAccount) == 0 {
		return nil
	}
	out := make(map[string]accountStat, len(rep.ByAccount))
	for _, row := range rep.ByAccount {
		if row.Name == "" {
			continue
		}
		out[row.Name] = accountStat{
			Requests:           row.Requests,
			Failures:           row.Failures,
			TotalTokens:        row.TotalTokens,
			AvgLatencyMs:       row.AvgLatencyMs,
			AvgTokensPerSecond: row.AvgTokensPerSecond,
			LastSuccess:        row.LastSuccess,
		}
	}
	return out
}
