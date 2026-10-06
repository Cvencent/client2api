package panel

import (
	"net/http"
	"time"

	"client2api/internal/core"
)

// quickconnect.go is the panel half of core.QuickConnectProvider: the
// "一键连接本机服务" block in the add-account dialog.  It is deliberately two
// small verbs rather than one call, because the operator has to see the probe
// before deciding anything:
//
//	POST <base>/quick-connect/<id>/probe     -> is the service up?
//	POST <base>/quick-connect/<id>/connect   -> create the source
//
// Probe writes nothing; Connect is the only call that creates a row.  A module
// that never opted in answers 501, the same "no button for an unimplemented
// mechanism" rule as every other optional capability.

// quickConnectRequest is the body of the connect call.  fields is open-ended
// on purpose: the panel sends whatever the target's card collected (today an
// optional API key and an optional base URL) and the module decides what it
// understands.
type quickConnectRequest struct {
	Fields map[string]string `json:"fields,omitempty"`
}

func (p *panel) quickConnectProbe(w http.ResponseWriter, r *http.Request, c core.Client, id string) {
	qc, ok := core.AsQuickConnectProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no one-click local sources")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	ctx, cancel := p.ctx(r, 15*time.Second)
	defer cancel()
	st, err := qc.ProbeQuickConnect(ctx, id)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (p *panel) quickConnectConnect(w http.ResponseWriter, r *http.Request, c core.Client, id string) {
	qc, ok := core.AsQuickConnectProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no one-click local sources")
		return
	}
	// The reply shape is the ordinary add-account one, so the panel can drop
	// the new row straight into the table it already has.  An AccountManager
	// is required for that relist; a module offering quick-connect without
	// one would be a programming error, not an operator error.
	am, ok := core.AsAccountManager(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" does not manage accounts")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body quickConnectRequest
	if err := optionalJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := p.ctx(r, 60*time.Second)
	defer cancel()
	rec, err := qc.ConnectQuickConnect(ctx, id, body.Fields)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account":  redactAccount(rec),
		"accounts": p.relist(ctx, am),
	})
}
