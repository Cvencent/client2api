package panel

import (
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

	bal, err := bp.AccountBalance(ctx, id, p.opts.ExpiringSoon)
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

	out := map[string]any{
		"ok":            true,
		"credits":       bal.Credits,
		"credits_total": bal.Total,
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
	Credits           int64   `json:"credits"`
	Used              float64 `json:"used,omitempty"`
	Total             int64   `json:"credits_total"`
	Expiring          int64   `json:"expiring,omitempty"`
	EarliestAt        string  `json:"earliest_at,omitempty"`
	EarliestRemaining int64   `json:"earliest_remaining,omitempty"`
	Error             string  `json:"error,omitempty"`

	// unit is the module's own label for Credits, collected here rather than on
	// the wire: it is a property of the module, not of a row, so the handler
	// hoists the first one it saw into the response's "unit" key.
	unit string
}

// balances implements GET <base>/balances: the live credit position of every
// account this module holds, which is what the accounts page renders in its
// 余额 column.
//
// It is a GET while the per-account route is a POST, and the asymmetry is
// deliberate.  The per-account button is an explicit "ask the vendor now" and
// must not be replayed by a browser or a proxy; this one is the accounts view's
// own read -- the column the operator asked to see -- and it is fetched when
// the view is opened, exactly like the account list beside it.  Both cost the
// same vendor calls, so the fan-out is bounded by the same packageFanout and
// one account failing is a row-level error, never a failed request.
func (p *panel) balances(w http.ResponseWriter, r *http.Request, c core.Client) {
	bp, ok := core.AsBalanceProvider(c)
	if !ok {
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
	ctx, cancel := p.ctx(r, 120*time.Second)
	defer cancel()

	recs, err := am.Accounts(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list accounts: "+core.Redact(err.Error()))
		return
	}

	rows := make([]balanceRow, len(recs))
	sem := make(chan struct{}, packageFanout)
	var wg sync.WaitGroup
	for i, rec := range recs {
		wg.Add(1)
		p.safeGo("panel balances", func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			row := balanceRow{ID: rec.ID, Label: rec.Label, State: rec.State}
			bal, err := bp.AccountBalance(ctx, rec.ID, p.opts.ExpiringSoon)
			if err != nil {
				row.Error = core.Redact(err.Error())
				rows[i] = row
				return
			}
			row.Credits, row.Total, row.Expiring = bal.Credits, bal.Total, bal.Expiring
			row.Used = bal.Used
			if !bal.EarliestAt.IsZero() {
				row.EarliestAt = bal.EarliestAt.Format(time.RFC3339)
				row.EarliestRemaining = bal.EarliestRemaining
			}
			row.unit = bal.Unit
			rows[i] = row
		})
	}
	wg.Wait()

	out := map[string]any{"accounts": rows}
	// The unit comes from the first row that named one, and a module whose every
	// read failed reports none -- the shell then prints a bare number rather
	// than a label it guessed.
	for _, row := range rows {
		if row.unit != "" {
			out["unit"] = row.unit
			break
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// packageRow is one account's line in the packages view.  The key names follow
// this dashboard's account table (id/label/state) rather than the reference's
// uid/nickname/realm, because in a multi-client panel "which module" is a
// column too and the browser already addresses accounts by "id".
type packageRow struct {
	ID       string               `json:"id"`
	Label    string               `json:"label,omitempty"`
	State    string               `json:"state,omitempty"`
	Remain   int64                `json:"remain"`
	Size     int64                `json:"size"`
	Packages []core.CreditPackage `json:"packages"`
	Error    string               `json:"error,omitempty"`
}

// packages implements GET <base>/packages: the per-tranche breakdown of every
// account this module holds, biggest balance first.  One account failing is a
// row-level error, never a failed request — the reference behaves the same way,
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
	var wg sync.WaitGroup
	for i, rec := range recs {
		wg.Add(1)
		p.safeGo("panel packages", func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			row := packageRow{ID: rec.ID, Label: rec.Label, State: rec.State}
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
