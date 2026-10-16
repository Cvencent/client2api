package raccoon

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"client2api/internal/core"
	"client2api/internal/smscap"
)

// sms.go implements core.SMSProvider for Raccoon.  The vendor's SMS login is a
// phone-code HTTP flow, so the module can rent a number from the one-time-SMS
// platform and read the code back without a human owning the phone.  The
// browser-driven part lives in autologin.go; this file is the platform half.

const (
	// defaultSMSProvider is the only one-time-SMS platform this module speaks.
	defaultSMSProvider = "eomsg"
	// defaultSMSKeyword is the sender the Raccoon verification text carries.
	// Asking for the wrong sender reads as "no message", which is why the
	// default is explicit instead of relying on the platform's own default.
	defaultSMSKeyword = "商汤科技"
)

// virtualPhonePrefixes are the virtual number ranges the eomsg pool can still
// return even when a real card was requested.  Raccoon's SMS route has the
// same delivery problems on those ranges, so a fresh draw hands them back.
var virtualPhonePrefixes = [...]string{"162", "165", "167", "170", "171"}

// smsResolved is one call's effective platform settings.  Precedence is
// override > config > environment.
type smsResolved struct {
	token     string
	keyword   string
	base      string
	proxy     string
	provinces []string
	cardType  string
}

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

func (c *Client) newSMSClient(r smsResolved) (*smscap.Client, error) {
	if r.token == "" {
		return nil, fmt.Errorf("no SMS platform token: set clients.raccoon.sms_token (or EOMSG_TOKEN), or paste one in the panel")
	}
	hc := c.deps.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: smscap.HTTPTimeout}
	}
	return smscap.New(smscap.Options{Base: r.base, Token: r.token, Keyword: r.keyword, HTTP: hc, Proxy: r.proxy})
}

// SMSStatus reports whether the module can rent a number right now.
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

// AcquirePhone rents one number.  want re-issues that exact number; otherwise a
// province is drawn from the rotation and any number in avoid is handed back.
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
		_ = sc.Release(ctx, num)
	}
	return core.SMSNumber{}, fmt.Errorf("the platform kept returning virtual or already-used numbers; try another province or card type")
}

// PollSMSCode reads the current message for one number.
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

// ReleasePhone hands the number back, or blacklists it when block is set.
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

func isVirtualSMSNumber(phone string) bool {
	phone = strings.TrimSpace(phone)
	for _, prefix := range virtualPhonePrefixes {
		if strings.HasPrefix(phone, prefix) {
			return true
		}
	}
	return false
}
