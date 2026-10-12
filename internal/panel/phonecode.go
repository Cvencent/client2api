package panel

import (
	"net/http"
	"strings"
	"time"

	"client2api/internal/core"
)

// phonecode.go is the panel half of core.PhoneCodeLoginProvider: the two-step
// "sign in with a number I own" flow.
//
//	POST <base>/phone-code/start   {phone}  -> ask the vendor to text a code
//	POST <base>/phone-code/finish  {id, code} -> redeem it and store the account
//
// The code is a one-run secret and the phone number belongs to the operator, so
// both travel in the request body, never in the URL.  A module that never opted
// in answers 501, exactly like every other optional capability.

type phoneCodeRequest struct {
	Phone string `json:"phone,omitempty"`
	ID    string `json:"id,omitempty"`
	Code  string `json:"code,omitempty"`
}

// phoneCodeStart answers POST <base>/phone-code/start.
func (p *panel) phoneCodeStart(w http.ResponseWriter, r *http.Request, c core.Client) {
	pp, ok := core.AsPhoneCodeLoginProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" cannot sign in with an operator-owned phone number")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body phoneCodeRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Phone) == "" {
		writeErr(w, http.StatusBadRequest, "phone is required")
		return
	}
	// One vendor round trip: the module asks the account service to text a code.
	ctx, cancel := p.ctx(r, 40*time.Second)
	defer cancel()
	challenge, err := pp.StartPhoneCodeLogin(ctx, strings.TrimSpace(body.Phone))
	if err != nil {
		writeErr(w, http.StatusBadGateway, core.Redact(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, challenge)
}

// phoneCodeFinish answers POST <base>/phone-code/finish.
func (p *panel) phoneCodeFinish(w http.ResponseWriter, r *http.Request, c core.Client) {
	pp, ok := core.AsPhoneCodeLoginProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" cannot sign in with an operator-owned phone number")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body phoneCodeRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.ID) == "" || strings.TrimSpace(body.Code) == "" {
		writeErr(w, http.StatusBadRequest, "id and code are required")
		return
	}
	ctx, cancel := p.ctx(r, 40*time.Second)
	defer cancel()
	record, err := pp.FinishPhoneCodeLogin(ctx, strings.TrimSpace(body.ID), strings.TrimSpace(body.Code))
	if err != nil {
		writeErr(w, http.StatusBadGateway, core.Redact(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, record)
}
