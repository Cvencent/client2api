package autoclaw

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"client2api/internal/core"
	"client2api/internal/smscap"
)

// sms.go is the module's one-time-SMS platform capability (core.SMSProvider).
//
// AutoClaw's vendor login is a plain HTTP phone-code exchange: the identity
// gateway texts a code to a number and redeems it for the account's tokens
// (see login.go).  Nothing about that step needs a browser, so the panel can
// rent a number here and either let the operator finish the loopback form or
// let the module run the whole flow itself (autologin.go).
//
// The platform protocol lives in internal/smscap: it is the same eomsg API
// workbuddy, loomy and qoder talk to, so every module shares one client.
//
// The platform rents numbers per project ("keyWord").  AutoClaw's codes are
// issued by 智谱华章, so that is the built-in keyword; an operator whose
// provider labels the sender differently can override it in the config or in
// the panel.

const (
	// defaultSMSKeyword is the project/sender keyword the eomsg platform
	// filters on for AutoClaw verification texts.
	defaultSMSKeyword = "智谱华章"
	// defaultSMSCardType keeps the platform's own default card class unless
	// the operator picked one.
	defaultSMSCardType = "全部"
)

// smsResolved is one call's effective platform settings.
type smsResolved struct {
	token     string
	keyword   string
	base      string
	proxy     string
	provinces []string
	cardType  string
}

// resolveSMS folds the module config and the per-call override into one
// effective setting.  Precedence is override > config > environment (the
// environment is applied to the config in New), which is what an operator
// expects: pasting a token in the panel beats the file, and the file beats a
// stale environment variable.
func (c *Client) resolveSMS(opts core.SMSOpts) smsResolved {
	r := smsResolved{
		token:    strings.TrimSpace(c.cfg.SMSToken),
		keyword:  strings.TrimSpace(c.cfg.SMSKeyword),
		base:     strings.TrimSpace(c.cfg.SMSBase),
		cardType: strings.TrimSpace(c.cfg.SMSCardType),
	}
	if t := strings.TrimSpace(opts.Token); t != "" {
		r.token = t
	}
	if p := strings.TrimSpace(opts.Proxy); p != "" {
		r.proxy = p
	}
	if k := strings.TrimSpace(opts.Keyword); k != "" {
		r.keyword = k
	}
	if r.keyword == "" {
		r.keyword = defaultSMSKeyword
	}
	if r.base == "" {
		r.base = smscap.DefaultBase
	}
	if ct := strings.TrimSpace(opts.CardType); ct != "" {
		r.cardType = ct
	}
	if r.cardType == "" {
		r.cardType = defaultSMSCardType
	}
	if len(c.cfg.SMSProvinces) > 0 {
		r.provinces = append([]string(nil), c.cfg.SMSProvinces...)
	} else {
		r.provinces = append([]string(nil), smscap.Provinces...)
	}
	r.provinces = smscap.CleanProvinces(r.provinces)
	return r
}

// newSMSClient builds the platform client for one call.  It refuses to build
// one without a token, which turns "not configured" into a clear error instead
// of an unauthenticated request.
func (c *Client) newSMSClient(r smsResolved) (*smscap.Client, error) {
	if r.token == "" {
		return nil, fmt.Errorf("no SMS platform token: set clients.autoclaw.sms_token (or EOMSG_TOKEN), or paste one in the panel")
	}
	hc := c.deps.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: smscap.HTTPTimeout}
	}
	return smscap.New(smscap.Options{Base: r.base, Token: r.token, Keyword: r.keyword, HTTP: hc, Proxy: r.proxy})
}

// ---------------------------------------------------------------------------
// core.SMSProvider
// ---------------------------------------------------------------------------

var _ core.SMSProvider = (*Client)(nil)

// SMSStatus reports whether the module can rent a number right now, and what
// the operator has to pick.  It reaches the platform only when a token is
// present: "not configured" is answered from local state, so the panel never
// makes an unauthenticated call just to draw its form.
func (c *Client) SMSStatus(ctx context.Context, opts core.SMSOpts) core.SMSStatus {
	r := c.resolveSMS(opts)
	st := core.SMSStatus{
		Configured: r.token != "",
		Provider:   "eomsg",
		Keyword:    r.keyword,
		Provinces:  r.provinces,
		CardTypes:  smscap.CardTypes,
	}
	if !st.Configured {
		return st
	}
	sc, err := c.newSMSClient(r)
	if err != nil {
		st.Error = core.Redact(err.Error())
		return st
	}
	ctx, cancel := context.WithTimeout(ctx, smscap.HTTPTimeout)
	defer cancel()
	if bal, err := sc.Balance(ctx); err == nil {
		st.Balance = strings.TrimSpace(bal)
	} else {
		st.Error = core.Redact(err.Error())
	}
	return st
}

// AcquirePhone rents one number.  want re-issues that exact number (the
// restore path, where the account's own number is known); otherwise a province
// is drawn from the rotation and any number in avoid is handed straight back
// and redrawn.
func (c *Client) AcquirePhone(ctx context.Context, opts core.SMSOpts, want string, avoid []string) (core.SMSNumber, error) {
	r := c.resolveSMS(opts)
	sc, err := c.newSMSClient(r)
	if err != nil {
		return core.SMSNumber{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, smscap.HTTPTimeout*2)
	defer cancel()

	if want = strings.TrimSpace(want); want != "" {
		num, err := sc.GetPhone(ctx, want, strings.TrimSpace(opts.Province), r.cardType)
		if err != nil {
			return core.SMSNumber{}, err
		}
		return core.SMSNumber{Phone: num, Province: strings.TrimSpace(opts.Province), Keyword: r.keyword, Reused: true}, nil
	}

	skip := make(map[string]bool, len(avoid))
	for _, n := range avoid {
		if n = strings.TrimSpace(n); n != "" {
			skip[n] = true
		}
	}
	province := strings.TrimSpace(opts.Province)
	for attempt := 0; attempt < c.cfg.dupRetries(); attempt++ {
		prov := province
		if prov == "" {
			prov = c.smsRot.Next(r.provinces)
		}
		num, err := sc.GetPhone(ctx, "", prov, r.cardType)
		if err != nil {
			return core.SMSNumber{}, err
		}
		if !skip[num] {
			return core.SMSNumber{Phone: num, Province: prov, Keyword: r.keyword}, nil
		}
		// Already in the caller's set: give it straight back so the platform
		// does not bill us for a number we will not use, then draw again.
		_ = sc.Release(ctx, num)
	}
	return core.SMSNumber{}, fmt.Errorf("the platform kept returning numbers already in use; try another province or card type")
}

// PollSMSCode reads the current SMS for one number.  Ready is false while the
// message has not arrived; a message that arrived without a parseable code is
// reported with Raw set so the operator can read it themselves.
func (c *Client) PollSMSCode(ctx context.Context, opts core.SMSOpts, phone string) (core.SMSCode, error) {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return core.SMSCode{}, fmt.Errorf("a phone number is required")
	}
	r := c.resolveSMS(opts)
	sc, err := c.newSMSClient(r)
	if err != nil {
		return core.SMSCode{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, smscap.HTTPTimeout)
	defer cancel()
	msg, err := sc.GetMsg(ctx, phone)
	if err != nil {
		return core.SMSCode{}, err
	}
	if msg == "" {
		return core.SMSCode{Ready: false}, nil
	}
	code := smscap.ExtractCode(msg)
	return core.SMSCode{Ready: code != "", Code: code, Raw: core.Redact(msg)}, nil
}

// ReleasePhone hands the number back, or blacklists it when block is set.  A
// failure here is reported but never hides the login outcome: the number's
// lease expires on its own either way.
func (c *Client) ReleasePhone(ctx context.Context, opts core.SMSOpts, phone string, block bool) error {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return fmt.Errorf("a phone number is required")
	}
	r := c.resolveSMS(opts)
	sc, err := c.newSMSClient(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, smscap.HTTPTimeout)
	defer cancel()
	if block {
		return sc.Block(ctx, phone)
	}
	return sc.Release(ctx, phone)
}
