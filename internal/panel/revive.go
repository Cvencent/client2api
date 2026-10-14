package panel

import (
	"context"
	"net/http"
	"time"

	"client2api/internal/core"
)

// reviveAccount implements POST <base>/accounts/<id>/revive.
//
// This is the panel half of the reference's accountRevive: clear whatever the
// module's own pool decided about one account.  The interesting part is the
// split between "the panel can do this" and "the module can":
//
//   - 501: the module implements no core.Reviver, i.e. it holds no runtime
//     penalty to clear.  Saying so is the honest answer; pretending to succeed
//     would leave the operator believing a broken account was fixed.
//   - 404: the module is a Reviver but does not know this id.  A stale browser
//     tab must not be able to report that it revived something that is gone.
//   - 400: anything else the module reported (an unwritable credential file,
//     for instance).  The module's message is relayed, redacted.
func (p *panel) reviveAccount(w http.ResponseWriter, r *http.Request, c core.Client, id string) {
	rv, ok := core.AsReviver(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no runtime penalties to clear")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	ctx, cancel := p.ctx(r, 30*time.Second)
	defer cancel()
	if err := rv.ReviveAccount(ctx, id); err != nil {
		// The panel cannot see into the module, so it asks the account list
		// whether this id exists at all: that is what separates "gone" from
		// "present but the revive itself failed".
		if !p.accountExists(ctx, c, id) {
			writeErr(w, http.StatusNotFound, "account not found")
			return
		}
		writeErr(w, http.StatusBadRequest, core.Redact(err.Error()))
		return
	}
	out := map[string]any{"ok": true}
	if am, ok := core.AsAccountManager(c); ok {
		out["accounts"] = p.relist(ctx, am)
	}
	writeJSON(w, http.StatusOK, out)
}

// accountExists reports whether the module still lists this id.  A module that
// cannot list accounts leaves the question open, and the caller then answers
// 400 rather than guessing 404: a wrong "not found" would send the operator
// looking for the wrong problem.
func (p *panel) accountExists(ctx context.Context, c core.Client, id string) bool {
	am, ok := core.AsAccountManager(c)
	if !ok {
		return true
	}
	recs, err := am.Accounts(ctx)
	if err != nil {
		return true
	}
	for _, rec := range recs {
		if rec.ID == id {
			return true
		}
	}
	return false
}

// setAccountModelPolicy implements
// POST <base>/accounts/<id>/model-policy.
//
// It writes one operator decision: which models this account may be routed to
// (a blacklist; absent means every model) and whether the account's account-level
// parks are force-enabled. It mirrors reviveAccount's 501/404/400 split so a
// stale tab cannot report success for a module or account that cannot mean it.
//
// A module that implements core.AccountModelPolicyManager stores the decision
// itself -- WorkBuddy needs to, because it knows which realm a model belongs to
// and can force an account-level park back to ready.  Every other module goes
// through the shared store the gateway's account hook reads, which is what
// lets the model dialog work on all platforms.  The force flag stays with the
// module: only it can override its own account-level parks, and a request that
// carries one anyway is ignored rather than half-honoured.
func (p *panel) setAccountModelPolicy(w http.ResponseWriter, r *http.Request, c core.Client, id string) {
	mp, moduleOwns := core.AsAccountModelPolicy(c)
	if !moduleOwns && p.opts.ModelPolicies == nil {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no per-account model policy")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body struct {
		DisabledModels []string `json:"disabled_models"`
		ForceEnabled   bool     `json:"force_enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	ctx, cancel := p.ctx(r, 30*time.Second)
	defer cancel()
	if moduleOwns {
		if err := mp.SetAccountModelPolicy(ctx, id, body.DisabledModels, body.ForceEnabled); err != nil {
			if !p.accountExists(ctx, c, id) {
				writeErr(w, http.StatusNotFound, "account not found")
				return
			}
			writeErr(w, http.StatusBadRequest, core.Redact(err.Error()))
			return
		}
	} else {
		// The store is keyed by a live account id, so a stale tab writing a
		// policy for an account that is gone is answered 404 exactly as the
		// module path would.
		if !p.accountExists(ctx, c, id) {
			writeErr(w, http.StatusNotFound, "account not found")
			return
		}
		if err := p.opts.ModelPolicies.Set(c.Name(), id, body.DisabledModels); err != nil {
			writeErr(w, http.StatusBadRequest, core.Redact(err.Error()))
			return
		}
	}
	out := map[string]any{"ok": true}
	if am, ok := core.AsAccountManager(c); ok {
		out["accounts"] = p.relist(ctx, am)
	}
	writeJSON(w, http.StatusOK, out)
}
