package panel

import (
	"context"
	"net/http"
	"time"

	"client2api/internal/core"
)

// overviewClient is one module's row in /panel/api/overview: its self-report,
// its accounts, and what the panel may ask of it.  It is a superset of the
// /panel/api/status entry, so the dashboard needs exactly one poll.
type overviewClient struct {
	Name         string               `json:"name"`
	Ready        bool                 `json:"ready"`
	Detail       string               `json:"detail"`
	Models       []string             `json:"models"`
	Accounts     []core.AccountRecord `json:"accounts"`
	Capabilities core.Capabilities    `json:"capabilities"`
	Error        string               `json:"error"`
}

type overview struct {
	Version   string `json:"version"`
	Uptime    string `json:"uptime"`
	StartedAt string `json:"started_at"`
	Requests  int64  `json:"requests"`
	Failures  int64  `json:"failures"`
	LogLines  int64  `json:"log_lines"`
	// Stats is the gateway's per-account traffic slice, keyed by account id.
	// It is the same map GET <base>/accounts returns under "stats", lifted
	// to the top level because it is one answer for the whole pool, not one
	// per client: copying it into every row would only inflate the payload
	// once per client for the same numbers.
	Stats map[string]accountStat `json:"stats"`
	// FullAccounts says Clients[].Accounts is the complete management list
	// and Stats carries the traffic slice, so this one response is enough to
	// render the accounts view.  Without it the shell has to re-ask every
	// module for the accounts it was just handed, which doubles the poll.
	FullAccounts bool `json:"full_accounts"`
	// AuthRequired tells the shell whether it has to ask for a key at all, so a
	// protected deployment can prompt before its first request fails.  The
	// dashboard also handles the 401/403 path, but a page that knows up front
	// never flashes an empty table first.  This is the reference's field name.
	AuthRequired bool             `json:"auth_required"`
	Clients      []overviewClient `json:"clients"`
}

// handleOverview is the dashboard's single polling endpoint.  It bounds the
// whole fan-out with one timeout, so one wedged module cannot hold the page.
func (p *panel) handleOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	ctx, cancel := p.ctx(r, 30*time.Second)
	defer cancel()

	out := overview{
		Version:      p.opts.Version,
		Uptime:       time.Since(p.opts.Started).Round(time.Second).String(),
		StartedAt:    p.opts.Started.Format(time.RFC3339),
		Requests:     p.opts.Stats.Requests(),
		Failures:     p.opts.Stats.Failures(),
		LogLines:     p.opts.Logs.Total(),
		AuthRequired: p.apiKey() != "",
		Stats:        p.overviewStats(),
		FullAccounts: true,
		Clients:      []overviewClient{},
	}
	if p.opts.Registry == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}

	for _, c := range p.opts.Registry.All() {
		out.Clients = append(out.Clients, p.overviewClient(ctx, c))
	}
	writeJSON(w, http.StatusOK, out)
}

// overviewStats is accountStats with the nil collapsed to an empty object, so
// the field is always present.  A page asking "can I skip the per-client round
// trips?" must be able to tell an old server (no field at all) from a new one
// with no traffic yet (an empty map), and only an explicit value can.
func (p *panel) overviewStats() map[string]accountStat {
	st := p.accountStats()
	if st == nil {
		return map[string]accountStat{}
	}
	return st
}

func (p *panel) overviewClient(ctx context.Context, c core.Client) overviewClient {
	st := core.RedactStatus(c.Status(ctx))

	// RedactStatus does not touch Status.Models, and a model id is a string
	// that could in principle carry a credential, so redact them by hand.
	models := make([]string, 0, len(st.Models))
	for _, m := range st.Models {
		models = append(models, core.Redact(m))
	}

	row := overviewClient{
		Name:         st.Name,
		Ready:        st.Ready,
		Detail:       st.Detail,
		Models:       models,
		Accounts:     []core.AccountRecord{},
		Capabilities: core.CapabilitiesOf(ctx, c),
	}

	// Accounts are only available from a module that manages them; a module
	// without that capability reports an empty list, not an error.
	if am, ok := core.AsAccountManager(c); ok {
		list, err := p.relistErr(ctx, am)
		if err != nil {
			row.Error = core.Redact(err.Error())
		}
		row.Accounts = list
	}
	return row
}
