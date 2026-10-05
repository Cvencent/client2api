package panel

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"client2api/internal/core"
)

// packageFanout bounds how many accounts one /packages request asks at once.
// The reference uses the same number (3): the vendor bills each package query
// as a normal API call, and a fleet of accounts must not turn one dashboard
// refresh into a burst.
const packageFanout = 3

// expiringSoon prefers the live window so a config reload moves the panel and
// the pools together; the startup value is only a fallback for embedders.
func (p *panel) expiringSoon() time.Duration {
	if p != nil && p.opts.Live != nil {
		if d := p.opts.Live.Load().ExpiringSoon; d > 0 {
			return d
		}
	}
	if p == nil {
		return 0
	}
	return p.opts.ExpiringSoon
}

// accountBalance implements POST <base>/accounts/<id>/balance.
//
// The reference answers {ok, credits, credits_total} and hands the extra
// buckets to its pool for "spend the expiring credit first" routing.  We relay
// them instead: our pools do not keep a credit ledger, and silently dropping
// numbers the vendor just told us would make the response a worse source of
// truth than the vendor is.  The keys are additive, so a client written
// against the reference still works.
func (p *panel) accountBalance(w http.ResponseWriter, r *http.Request, c core.Client, id string) {
	bp, ok := core.AsBalanceProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" cannot read an account balance")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	ctx, cancel := p.ctx(r, 60*time.Second)
	defer cancel()

	bal, err := bp.AccountBalance(ctx, id, p.expiringSoon())
	if err != nil {
		// Same split as revive: a stale id is "not found", a live account that
		// the vendor refused is an upstream failure (the reference's 502).
		if !p.accountExists(ctx, c, id) {
			writeErr(w, http.StatusNotFound, "account not found")
			return
		}
		writeErr(w, http.StatusBadGateway, "user resource: "+core.Redact(err.Error()))
		return
	}

	// A successful on-demand read is also the freshest thing the column can
	// show, so keep it for the next page load instead of immediately
	// forgetting a number we already asked the vendor for.
	if p.balanceCache != nil {
		p.balanceCache.put(c.Name(), id, bal)
	}

	out := map[string]any{
		"ok":            true,
		"credits":       bal.Credits,
		"credits_total": bal.Total,
	}
	if bal.Unlimited {
		out["unlimited"] = true
	}
	if bal.Used > 0 {
		out["used"] = bal.Used
	}
	// What the number counts, in the module's own words.  Omitted when the
	// module did not say, so a client written against the reference sees the
	// shape it always saw.
	if bal.Unit != "" {
		out["unit"] = bal.Unit
	}
	if bal.Expiring > 0 {
		out["expiring"] = bal.Expiring
	}
	if !bal.EarliestAt.IsZero() {
		out["earliest_at"] = bal.EarliestAt.Format(time.RFC3339)
		out["earliest_remaining"] = bal.EarliestRemaining
	}
	if am, ok := core.AsAccountManager(c); ok {
		out["accounts"] = p.relist(ctx, am)
	}
	writeJSON(w, http.StatusOK, out)
}

// balanceRow is one account's line in the accounts page's balance column.
//
// It carries the same numbers as the per-account route, so the shell has one
// shape to read either way, and it deliberately does NOT carry the account
// list: the accounts page already has that list, and repeating it here would
// make a fleet-wide read look like a state change.
type balanceRow struct {
	ID                string  `json:"id"`
	Label             string  `json:"label,omitempty"`
	State             string  `json:"state,omitempty"`
	Credits           *int64  `json:"credits,omitempty"`
	Used              float64 `json:"used,omitempty"`
	Total             int64   `json:"credits_total"`
	Unlimited         bool    `json:"unlimited,omitempty"`
	Expiring          int64   `json:"expiring,omitempty"`
	EarliestAt        string  `json:"earliest_at,omitempty"`
	EarliestRemaining int64   `json:"earliest_remaining,omitempty"`
	Error             string  `json:"error,omitempty"`
	FetchedAt         int64   `json:"fetched_at,omitempty"`
	Unit              string  `json:"unit,omitempty"`
}

// balances implements GET <base>/balances: the last known credit position of
// every account this module holds, which is what the accounts page renders in
// its 余额 column.
//
// It is a GET while the per-account route is a POST, and the asymmetry is
// deliberate.  The per-account button is an explicit "ask the vendor now" and
// must not be replayed by a browser or a proxy.  This read, by contrast, is
// served from the cache so that opening the page does not fan out over the
// whole pool every time; it schedules one paced background pass instead.
//
// A module without BalanceProvider still answers 501, so the column stays
// hidden.  A row that has never been read has no "credits" key at all rather
// than a zero, because zero is a real balance and the shell must not show it
// for an account the vendor has not answered about yet.
func (p *panel) balances(w http.ResponseWriter, r *http.Request, c core.Client) {
	if _, ok := core.AsBalanceProvider(c); !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" cannot read an account balance")
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	am, ok := core.AsAccountManager(c)
	if !ok {
		// Without a list there is nothing to iterate.  Saying so is better than
		// inventing a single anonymous row.
		writeErr(w, http.StatusNotImplemented, c.Name()+" cannot list accounts")
		return
	}
	ctx, cancel := p.ctx(r, 30*time.Second)
	defer cancel()

	recs, err := am.Accounts(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list accounts: "+core.Redact(err.Error()))
		return
	}

	snapshot := map[string]balanceEntry{}
	if p.balanceCache != nil {
		snapshot = p.balanceCache.snapshot(c.Name())
	}

	rows := make([]balanceRow, 0, len(recs))
	for _, rec := range recs {
		e, known := snapshot[rec.ID]
		row := balanceRow{ID: rec.ID, Label: rec.Label, State: rec.State}
		if known {
			row.FetchedAt = e.FetchedAt
			row.Unit = e.Unit
			row.Error = e.Error
			if e.Error == "" {
				credits := e.Credits
				row.Credits = &credits
				row.Total = e.Total
				row.Used = e.Used
				row.Unlimited = e.Unlimited
				row.Expiring = e.Expiring
				row.EarliestAt = e.EarliestAt
				row.EarliestRemaining = e.EarliestRemaining
			}
		}
		rows = append(rows, row)
	}

	out := map[string]any{"accounts": rows}
	if unit := unitOf(snapshot); unit != "" {
		out["unit"] = unit
	}
	if p.balanceCache != nil {
		// Fire and forget from a context that outlives this request: the
		// vendor calls must not be cancelled the moment the GET returns.
		p.balanceCache.refresh(context.WithoutCancel(r.Context()), balanceRefreshBatch, false)
	}
	writeJSON(w, http.StatusOK, out)
}

// balancesRefresh implements POST <base>/balances/refresh, the explicit
// "刷新余额" toolbar action.  It answers with the cache immediately and
// asks the vendor for a small, spaced batch in the background.
func (p *panel) balancesRefresh(w http.ResponseWriter, r *http.Request, c core.Client) {
	if _, ok := core.AsBalanceProvider(c); !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" cannot read an account balance")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if _, ok := core.AsAccountManager(c); !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" cannot list accounts")
		return
	}
	started := false
	if p.balanceCache != nil {
		started = p.balanceCache.refresh(context.WithoutCancel(r.Context()), balanceRefreshBatch, true)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": started, "batch": balanceRefreshBatch})
}

// packageRow is one account's line in the packages view.  The key names follow
// this dashboard's account table (id/label/state) rather than the reference's
// uid/nickname/realm, because in a multi-client panel "which module" is a
// column too and the browser already addresses accounts by "id".
type packageRow struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
	// Identity and OperatorNote let the credits view merge the rows that are
	// really one vendor account reached through several credentials.  The
	// accounts view has grouped on these for a while; the packages view served
	// the same account once per channel, which is where "多个 Zcode" came from.
	Identity     string               `json:"identity,omitempty"`
	OperatorNote string               `json:"operator_note,omitempty"`
	State        string               `json:"state,omitempty"`
	Remain       int64                `json:"remain"`
	Size         int64                `json:"size"`
	Packages     []core.CreditPackage `json:"packages"`
	Error        string               `json:"error,omitempty"`
}

// packages implements GET <base>/packages: the per-tranche breakdown of every
// account this module holds, biggest balance first.  One account failing is a
// row-level error, never a failed request; the reference behaves the same way,
// because the interesting comparison is between the accounts that answered.
func (p *panel) packages(w http.ResponseWriter, r *http.Request, c core.Client) {
	pp, ok := core.AsPackageProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" does not report credit packages")
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	am, ok := core.AsAccountManager(c)
	if !ok {
		// Without a list there is nothing to iterate.  Saying so is better than
		// inventing a single anonymous row.
		writeErr(w, http.StatusNotImplemented, c.Name()+" cannot list accounts")
		return
	}
	ctx, cancel := p.ctx(r, 120*time.Second)
	defer cancel()

	recs, err := am.Accounts(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list accounts: "+core.Redact(err.Error()))
		return
	}

	rows := make([]packageRow, len(recs))
	sem := make(chan struct{}, packageFanout)
	// The group keys travel with the rows: identity is the module's own answer
	// about who owns the credential, and the operator note is the human name
	// the accounts view already shows for it.
	p.decorateAccountNotes(c.Name(), recs)
	var wg sync.WaitGroup
	for i, rec := range recs {
		wg.Add(1)
		p.safeGo("panel packages", func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			row := packageRow{ID: rec.ID, Label: rec.Label, State: rec.State, Identity: rec.Identity, OperatorNote: rec.OperatorNote}
			rep, err := pp.AccountPackages(ctx, rec.ID)
			if err != nil {
				row.Error = core.Redact(err.Error())
				rows[i] = row
				return
			}
			row.Remain, row.Size = rep.Remain, rep.Size
			if rep.Packages == nil {
				// An empty breakdown is a fact; a JSON null reads as "unknown".
				row.Packages = []core.CreditPackage{}
			} else {
				row.Packages = rep.Packages
			}
			rows[i] = row
		})
	}
	wg.Wait()

	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Remain > rows[j].Remain })
	writeJSON(w, http.StatusOK, map[string]any{"accounts": rows})
}

// voucherRow is one account's vouchers in the school/vouchers view.
type voucherRow struct {
	ID       string         `json:"id"`
	Label    string         `json:"label,omitempty"`
	Vouchers []core.Voucher `json:"vouchers"`
	Error    string         `json:"error,omitempty"`
}

// schoolVouchers implements GET <base>/school/vouchers.
//
// The reference is single-vendor, so its route lists the one account's prizes.
// Ours walks the module's accounts under the same fan-out bound: the codes are
// what an operator comes here for, and they are per account.
func (p *panel) schoolVouchers(w http.ResponseWriter, r *http.Request, c core.Client) {
	vp, ok := core.AsVoucherProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no vouchers to list")
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	am, ok := core.AsAccountManager(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" cannot list accounts")
		return
	}
	ctx, cancel := p.ctx(r, 120*time.Second)
	defer cancel()

	recs, err := am.Accounts(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list accounts: "+core.Redact(err.Error()))
		return
	}

	rows := make([]voucherRow, len(recs))
	sem := make(chan struct{}, packageFanout)
	var wg sync.WaitGroup
	for i, rec := range recs {
		wg.Add(1)
		p.safeGo("panel school vouchers", func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			row := voucherRow{ID: rec.ID, Label: rec.Label}
			vs, err := vp.AccountVouchers(ctx, rec.ID)
			if err != nil {
				row.Error = core.Redact(err.Error())
				rows[i] = row
				return
			}
			// Codes are shown verbatim, not redacted: a redemption code IS the
			// payload of this view, and an operator has to be able to copy it.
			// Redaction is for credentials that leaked into text by accident --
			// running it over an intentional value would break `secret_…`-shaped
			// codes for no gain.
			row.Vouchers = make([]core.Voucher, 0, len(vs))
			row.Vouchers = append(row.Vouchers, vs...)
			rows[i] = row
		})
	}
	wg.Wait()

	total := 0
	for _, row := range rows {
		total += len(row.Vouchers)
	}
	out := map[string]any{"accounts": rows, "count": total}
	writeJSON(w, http.StatusOK, out)
}
