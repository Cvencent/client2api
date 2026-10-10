package qoder

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"client2api/internal/core"
	"client2api/internal/smscap"
)

// sms.go is the module's one-time-SMS platform capability (core.SMSProvider).
//
// Qoder CN does not register a phone number itself: its sign-in page hands the
// operator off to Alibaba Cloud SSO, whose phone form texts a code and either
// signs the number in or creates the account behind it.  A browser can drive
// that form, so this module can add an account without the operator owning the
// phone -- see autologin.go for the end-to-end run.  This file rents the number
// and reads the code back from the platform.
//
// The platform protocol lives in internal/smscap: it is the same eomsg API
// workbuddy and loomy talk to, so all three modules share one client.
//
// The keyword matters more here than anywhere else in the project: the vendor's
// text is signed "qoder", NOT "阿里云".  Asking the platform for the wrong
// sender reads as "no message has arrived" even though the code was delivered,
// which is exactly the failure this default exists to prevent.

const (
	// defaultSMSProvider is the only one-time-SMS platform this module speaks.
	defaultSMSProvider = "eomsg"
)

// virtualPhonePrefixes are the virtual number ranges the eomsg pool can still
// return even when the operator asks for a real card.  Qoder's Aliyun SMS
// route frequently drops those messages, so a fresh draw hands them straight
// back instead of burning the run's SMS wait on a number that cannot receive.
var virtualPhonePrefixes = [...]string{"162", "165", "167", "170", "171"}

// smsResolved is one call's effective platform settings.
type smsResolved struct {
	token     string
	keyword   string
	base      string
	provinces []string
	cardType  string
}

// resolveSMS folds the module config, the process environment and the per-call
// override into one effective setting.  Precedence is override > config > env,
// which is what an operator expects: pasting a token in the panel beats the
// file, and the file beats a stale environment variable.
func (c *Client) resolveSMS(opts core.SMSOpts) smsResolved {
	r := smsResolved{
		token:    strings.TrimSpace(c.cfg.SMSToken),
		keyword:  strings.TrimSpace(c.cfg.SMSKeyword),
		base:     strings.TrimSpace(c.cfg.SMSBase),
		cardType: c.cfg.smsCardType(),
	}
	if r.token == "" {
		r.token = strings.TrimSpace(os.Getenv("EOMSG_TOKEN"))
	}
	if t := strings.TrimSpace(opts.Token); t != "" {
		r.token = t
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
		r.cardType = "全部"
	}
	if len(c.cfg.SMSProvinces) > 0 {
		r.provinces = append([]string(nil), c.cfg.SMSProvinces...)
	} else {
		r.provinces = append([]string(nil), smscap.Provinces...)
	}
	r.provinces = smscap.CleanProvinces(r.provinces)
	return r
}

// newSMSClient builds the platform client for one call.  It refuses to build one
// without a token, which turns "not configured" into a clear error instead of an
// unauthenticated request.
func (c *Client) newSMSClient(r smsResolved) (*smscap.Client, error) {
	if r.token == "" {
		return nil, fmt.Errorf("no SMS platform token: set clients.qoder.sms_token (or EOMSG_TOKEN), or paste one in the panel")
	}
	hc := c.deps.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: smscap.HTTPTimeout}
	}
	return smscap.New(smscap.Options{Base: r.base, Token: r.token, Keyword: r.keyword, HTTP: hc})
}

// ---------------------------------------------------------------------------
// core.SMSProvider
// ---------------------------------------------------------------------------

var _ core.SMSProvider = (*Client)(nil)

// SMSStatus reports whether the module can rent a number right now, and what the
// operator has to pick.  It reaches the platform only when a token is present:
// "not configured" is answered from local state, so the panel never makes an
// unauthenticated call just to draw its form.
func (c *Client) SMSStatus(ctx context.Context, opts core.SMSOpts) core.SMSStatus {
	r := c.resolveSMS(opts)
	st := core.SMSStatus{
		Configured: r.token != "",
		Provider:   defaultSMSProvider,
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

// AcquirePhone rents one number.  want re-issues that exact number (the restore
// path, where the account's own number is known); otherwise a province is drawn
// from the rotation and any number in avoid is handed straight back and redrawn.
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
		if isVirtualSMSNumber(num) {
			_ = sc.Release(ctx, num)
			continue
		}
		if !skip[num] {
			return core.SMSNumber{Phone: num, Province: prov, Keyword: r.keyword}, nil
		}
		// Already in the caller's set: give it straight back so the platform
		// does not bill for a number we will not use, then draw again.
		_ = sc.Release(ctx, num)
	}
	return core.SMSNumber{}, fmt.Errorf("the platform kept returning virtual or already-used numbers; try another province or card type")
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
// failure here is reported but never hides the login outcome: the number's lease
// expires on its own either way.
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

// isVirtualSMSNumber reports whether a fresh draw landed on a virtual range.
// The platform's cardType filter is not reliable, so this is a second guard.
func isVirtualSMSNumber(phone string) bool {
	phone = strings.TrimSpace(phone)
	for _, prefix := range virtualPhonePrefixes {
		if strings.HasPrefix(phone, prefix) {
			return true
		}
	}
	return false
}
