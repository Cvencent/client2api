package panel

import (
	"net/http"
	"strings"
	"time"

	"client2api/internal/core"
)

// autologin.go is the panel half of core.AutoLoginProvider: the one-click
// "自动添加" that drives the vendor's own login page in a browser the module
// launches, instead of handing the operator a link and asking them to type a
// number and a code.
//
//	POST   <base>/auto-login          start a run
//	GET    <base>/auto-login/<id>     read its progress
//	DELETE <base>/auto-login/<id>     stop it
//
// The platform token is a credential, so it travels in the request body, never
// in the URL, and the module never echoes it back.  A module that never opted
// in answers 501, exactly like every other optional capability.

// autoLoginRequest is the start body.  Every field is optional: an empty realm
// means the module's configured default and empty platform fields mean its
// configured settings.  Token is the operator's per-run override of the
// platform credential, the same way smsRequest carries one.
type autoLoginRequest struct {
	Realm    string `json:"realm,omitempty"`
	Token    string `json:"token,omitempty"`
	Keyword  string `json:"keyword,omitempty"`
	Province string `json:"province,omitempty"`
	CardType string `json:"card_type,omitempty"`
	Phone    string `json:"phone,omitempty"`
}

// autoLoginStart answers POST <base>/auto-login.
func (p *panel) autoLoginStart(w http.ResponseWriter, r *http.Request, c core.Client) {
	ap, ok := core.AsAutoLoginProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" cannot run its login automatically")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body autoLoginRequest
	if err := optionalJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// The start call validates the prerequisites and returns immediately, so a
	// generous ceiling here only covers the vendor's auth-state call.
	ctx, cancel := p.ctx(r, 30*time.Second)
	defer cancel()
	job, err := ap.StartAutoLogin(ctx, core.AutoLoginRequest{
		Realm:    strings.TrimSpace(body.Realm),
		Token:    strings.TrimSpace(body.Token),
		Keyword:  strings.TrimSpace(body.Keyword),
		Province: strings.TrimSpace(body.Province),
		CardType: strings.TrimSpace(body.CardType),
		Phone:    strings.TrimSpace(body.Phone),
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, core.Redact(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, redactAutoJob(job))
}

// autoLoginByID answers GET and DELETE <base>/auto-login/<id>.  A GET never
// touches the network, so the panel may poll it as often as it likes.
func (p *panel) autoLoginByID(w http.ResponseWriter, r *http.Request, c core.Client, id string) {
	ap, ok := core.AsAutoLoginProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" cannot run its login automatically")
		return
	}
	ctx, cancel := p.ctx(r, 20*time.Second)
	defer cancel()
	switch r.Method {
	case http.MethodGet:
		job, err := ap.PollAutoLogin(ctx, id)
		if err != nil {
			writeErr(w, http.StatusNotFound, core.Redact(err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, redactAutoJob(job))
	case http.MethodDelete:
		if err := ap.CancelAutoLogin(ctx, id); err != nil {
			writeErr(w, http.StatusBadGateway, core.Redact(err.Error()))
			return
		}
		// Answer with the job's own state so the caller learns what actually
		// happened, rather than assuming the cancel won the race.
		job, err := ap.PollAutoLogin(ctx, id)
		if err != nil {
			writeErr(w, http.StatusBadGateway, core.Redact(err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, redactAutoJob(job))
	default:
		writeErr(w, http.StatusMethodNotAllowed, "use GET or DELETE")
	}
}

// redactAutoJob scrubs a job the way every other panel payload is scrubbed.
// The log is already redacted where it is written; this is the second net.
func redactAutoJob(job core.AutoLoginJob) core.AutoLoginJob {
	job.Message = core.Redact(job.Message)
	job.AccountID = core.Redact(job.AccountID)
	job.Phone = core.Redact(job.Phone)
	if job.Log == nil {
		job.Log = []core.AutoLogLine{}
		return job
	}
	lines := make([]core.AutoLogLine, len(job.Log))
	for i, l := range job.Log {
		l.Text = core.Redact(l.Text)
		lines[i] = l
	}
	job.Log = lines
	return job
}
