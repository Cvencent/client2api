package panel

import (
	"net/http"
	"strings"
	"time"

	"client2api/internal/core"
)

// invite.go exposes the module's invite-code and redemption-code surfaces:
//
//	GET  <base>/invites              default code + per-account memo
//	POST <base>/invites/bind         {"account":"…","code":"…"}   (account optional)
//	POST <base>/accounts/<id>/redeem {"code":"…"}
//
// "account" omitted on bind means "every enabled account" and is what the
// toolbar action sends.  Binding one account at a time keeps a vendor refusal
// on that row instead of turning the whole pool into one failed request.

type inviteAccountRow struct {
	ID        string `json:"id"`
	Label     string `json:"label,omitempty"`
	Enabled   bool   `json:"enabled"`
	Code      string `json:"code,omitempty"`
	Activated bool   `json:"activated,omitempty"`
	OK        bool   `json:"ok,omitempty"`
	Already   bool   `json:"already,omitempty"`
	Reward    int64  `json:"reward,omitempty"`
	Message   string `json:"message,omitempty"`
	Error     string `json:"error,omitempty"`
}

// invites reads the default code and the last known per-account binding.  It
// never touches the vendor: activation status is cached after each bind so the
// accounts page can render without a fleet-wide burst.
func (p *panel) invites(w http.ResponseWriter, r *http.Request, c core.Client) {
	ip, ok := core.AsInviteCodeProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no invite-code support")
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
	ctx, cancel := p.ctx(r, 30*time.Second)
	defer cancel()
	recs, err := am.Accounts(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list accounts: "+core.Redact(err.Error()))
		return
	}
	rows := make([]inviteAccountRow, 0, len(recs))
	for _, rec := range recs {
		row := inviteAccountRow{ID: rec.ID, Label: rec.Label, Enabled: rec.Enabled}
		if rec.Fields != nil {
			row.Code, _ = rec.Fields["invite_code"].(string)
			row.Activated, _ = rec.Fields["invite_activated"].(bool)
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"default_code": ip.InviteCodeDefault(ctx),
		"accounts":     rows,
	})
}

// bindInvites handles both "bind this account" and "bind every enabled
// account".  The request is synchronous because it is an operator action with
// an explicit progress label in the shell; the account list is small and the
// endpoint is idempotent.
func (p *panel) bindInvites(w http.ResponseWriter, r *http.Request, c core.Client) {
	ip, ok := core.AsInviteCodeProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no invite-code support")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	am, ok := core.AsAccountManager(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" cannot list accounts")
		return
	}
	var body struct {
		Account string `json:"account"`
		Code    string `json:"code"`
	}
	if !decodeJSON(w, r, &body) { // optional body; empty selects the module default
		return
	}
	// A fleet-wide sweep is only offered to modules that say it is safe. Trae's
	// new-user-only campaign refuses every pre-existing account, so a caller
	// that ignores the capability flag is told so instead of being handed a
	// wall of 9022 refusals.
	if strings.TrimSpace(body.Account) == "" {
		if fb, ok := core.AsInviteCodeFleetBinder(c); ok && !fb.InviteCodeBindAllSafe() {
			writeErr(w, http.StatusBadRequest, c.Name()+" 的邀请码只对新注册账号生效，不能批量绑定已有账号")
			return
		}
	}
	ctx, cancel := p.ctx(r, 180*time.Second)
	defer cancel()

	recs, err := am.Accounts(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list accounts: "+core.Redact(err.Error()))
		return
	}
	wanted := strings.TrimSpace(body.Account)
	if wanted != "" {
		found := false
		for _, rec := range recs {
			if rec.ID == wanted {
				found = true
				break
			}
		}
		if !found {
			writeErr(w, http.StatusNotFound, "account not found")
			return
		}
	}

	results := make([]core.InviteCodeResult, 0, len(recs))
	for _, rec := range recs {
		if wanted != "" && rec.ID != wanted {
			continue
		}
		if wanted == "" && !rec.Enabled {
			continue
		}
		res, err := ip.BindAccountInviteCode(ctx, rec.ID, strings.TrimSpace(body.Code))
		if err != nil {
			res = core.InviteCodeResult{AccountID: rec.ID, Code: strings.TrimSpace(body.Code), Error: core.Redact(err.Error())}
		}
		res.Error = core.Redact(res.Error)
		res.Message = core.Redact(res.Message)
		results = append(results, res)
	}
	okCount, already, failed := 0, 0, 0
	for _, res := range results {
		switch {
		case res.OK && res.Already:
			okCount++
			already++
		case res.OK:
			okCount++
		default:
			failed++
		}
	}
	out := map[string]any{
		"default_code": ip.InviteCodeDefault(ctx),
		"results":      results,
		"ok":           okCount,
		"already":      already,
		"failed":       failed,
	}
	if wanted == "" {
		out["accounts"] = p.relist(ctx, am)
	} else {
		out["account"] = wanted
		out["accounts"] = p.relist(ctx, am)
	}
	writeJSON(w, http.StatusOK, out)
}

// redeemAccountCode implements POST <base>/accounts/<id>/redeem.  It is the
// second points-page action and takes a redemption code, not an invite code.
func (p *panel) redeemAccountCode(w http.ResponseWriter, r *http.Request, c core.Client, id string) {
	ip, ok := core.AsInviteCodeProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no redemption-code support")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	ctx, cancel := p.ctx(r, 90*time.Second)
	defer cancel()
	res, err := ip.RedeemAccountCode(ctx, id, strings.TrimSpace(body.Code))
	if err != nil {
		if !p.accountExists(ctx, c, id) {
			writeErr(w, http.StatusNotFound, "account not found")
			return
		}
		writeErr(w, http.StatusBadGateway, core.Redact(err.Error()))
		return
	}
	res.Error = core.Redact(res.Error)
	res.Message = core.Redact(res.Message)
	out := map[string]any{"result": res}
	if am, ok := core.AsAccountManager(c); ok {
		out["accounts"] = p.relist(ctx, am)
	}
	writeJSON(w, http.StatusOK, out)
}
