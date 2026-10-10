package panel

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"client2api/internal/core"
)

// sms.go is the panel half of core.SMSProvider: the "接码" controls that let an
// operator add a WorkBuddy account with a rented phone number instead of their
// own.
//
// The routes are deliberately four small verbs rather than one "add an
// account" call, because the vendor's login page sits between them and only the
// operator can drive it:
//
//	GET  <base>/sms             -> status (configured? balance? provinces?)
//	POST <base>/sms/phone       -> rent a number (or re-issue one)
//	POST <base>/sms/code        -> poll once for the SMS code
//	POST <base>/sms/release     -> hand the number back, or blacklist it
//
// A module that never opted in answers 501, exactly like every other optional
// capability: no button for something that cannot be done.
//
// The platform token is a credential, so it travels in the request body, never
// in the URL, and the module never echoes it back.  An operator may paste one
// per request; the module treats it as an override of its configured token.

// smsRequest is the shared body of the three mutating routes.  Every field is
// optional: the panel sends only what the operator changed.
type smsRequest struct {
	Token    string   `json:"token,omitempty"`
	Proxy    string   `json:"proxy,omitempty"`
	Keyword  string   `json:"keyword,omitempty"`
	Province string   `json:"province,omitempty"`
	CardType string   `json:"card_type,omitempty"`
	Phone    string   `json:"phone,omitempty"`
	Block    bool     `json:"block,omitempty"`
	Avoid    []string `json:"avoid,omitempty"`
}

func (b smsRequest) opts() core.SMSOpts {
	return core.SMSOpts{
		Token:    strings.TrimSpace(b.Token),
		Proxy:    strings.TrimSpace(b.Proxy),
		Keyword:  strings.TrimSpace(b.Keyword),
		Province: strings.TrimSpace(b.Province),
		CardType: strings.TrimSpace(b.CardType),
	}
}

// smsStatus answers GET <base>/sms (and POST, so a pasted token can be checked
// without restarting the process).
func (p *panel) smsStatus(w http.ResponseWriter, r *http.Request, c core.Client) {
	sp, ok := core.AsSMSProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no SMS platform support")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use GET or POST")
		return
	}
	var body smsRequest
	if err := optionalJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := p.ctx(r, 30*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, sp.SMSStatus(ctx, body.opts()))
}

// smsPhone answers POST <base>/sms/phone.
func (p *panel) smsPhone(w http.ResponseWriter, r *http.Request, c core.Client) {
	sp, ok := core.AsSMSProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no SMS platform support")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body smsRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	ctx, cancel := p.ctx(r, 60*time.Second)
	defer cancel()
	num, err := sp.AcquirePhone(ctx, body.opts(), body.Phone, body.Avoid)
	if err != nil {
		writeErr(w, http.StatusBadGateway, core.Redact(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, num)
}

// smsCode answers POST <base>/sms/code.  A poll that has not seen the message
// yet is a 200 with ready=false -- the panel keeps polling; only a platform
// failure is an error.
func (p *panel) smsCode(w http.ResponseWriter, r *http.Request, c core.Client) {
	sp, ok := core.AsSMSProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no SMS platform support")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body smsRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Phone) == "" {
		writeErr(w, http.StatusBadRequest, "phone is required")
		return
	}
	ctx, cancel := p.ctx(r, 40*time.Second)
	defer cancel()
	code, err := sp.PollSMSCode(ctx, body.opts(), body.Phone)
	if err != nil {
		writeErr(w, http.StatusBadGateway, core.Redact(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, code)
}

// smsRelease answers POST <base>/sms/release.  A release that fails is reported
// but is not fatal to the caller: the platform reclaims the number when its
// lease expires anyway.
func (p *panel) smsRelease(w http.ResponseWriter, r *http.Request, c core.Client) {
	sp, ok := core.AsSMSProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no SMS platform support")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body smsRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Phone) == "" {
		writeErr(w, http.StatusBadRequest, "phone is required")
		return
	}
	ctx, cancel := p.ctx(r, 30*time.Second)
	defer cancel()
	if err := sp.ReleasePhone(ctx, body.opts(), body.Phone, body.Block); err != nil {
		writeErr(w, http.StatusBadGateway, core.Redact(err.Error()))
		return
	}
	action := "released"
	if body.Block {
		action = "blocked"
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": action, "phone": body.Phone})
}

// optionalJSONBody decodes a body that may be absent or empty.  Unlike
// decodeJSON it does not write a response: the caller decides what an empty
// body means (here: "use the configured defaults").
func optionalJSONBody(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<10))
	if err != nil {
		return errors.New("the request body could not be read")
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return errors.New("the request body must be JSON")
	}
	return nil
}
