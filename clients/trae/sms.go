package trae

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"client2api/internal/core"
	"client2api/internal/smscap"
)

// sms.go implements the one-time-SMS capability (core.SMSProvider) the auto
// login needs.  The Trae sign-in page accepts a phone number, so the panel can
// rent a number from the shared eomsg service, read the code, and let the
// auto-login driver submit it — the same experience the officeace, codearts
// and raccoon modules already have.
//
// The platform token follows the same layering as the other modules:
// config (clients.trae.sms_token) beats EOMSG_TOKEN, and a per-request panel
// override beats both.  A missing token means the capability is visible but
// reports "not configured" instead of making an unauthenticated request.

const (
	traeSMSProvider     = "eomsg"
	traeDefaultSMSKey   = "华为"
	traeDefaultCardType = "全部"
	traeDupRetries      = 6
)

type traeSMSResolved struct {
	token     string
	keyword   string
	base      string
	proxy     string
	provinces []string
	cardType  string
}

// resolveSMS folds the request override, the module config and the process
// environment into one effective set of platform settings.
func (c *Client) resolveSMS(opts core.SMSOpts) traeSMSResolved {
	r := traeSMSResolved{
		token:    strings.TrimSpace(c.cfg.SMS.Token),
		keyword:  strings.TrimSpace(c.cfg.SMS.Keyword),
		base:     strings.TrimSpace(c.cfg.SMS.Base),
		cardType: strings.TrimSpace(c.cfg.SMS.CardType),
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
		r.keyword = traeDefaultSMSKey
	}
	if ct := strings.TrimSpace(opts.CardType); ct != "" {
		r.cardType = ct
	}
	if r.cardType == "" {
		r.cardType = traeDefaultCardType
	}
	if r.base == "" {
		r.base = smscap.DefaultBase
	}
	if len(c.cfg.SMS.Provinces) > 0 {
		r.provinces = append([]string(nil), c.cfg.SMS.Provinces...)
	} else {
		r.provinces = append([]string(nil), smscap.Provinces...)
	}
	r.provinces = smscap.CleanProvinces(r.provinces)
	return r
}

func (c *Client) newSMSClient(r traeSMSResolved) (*smscap.Client, error) {
	if strings.TrimSpace(r.token) == "" {
		return nil, fmt.Errorf("no SMS platform token: set clients.trae.sms_token (or EOMSG_TOKEN), or paste one in the panel")
	}
	var hc *http.Client
	if c != nil {
		hc = c.httpClient
	}
	if hc == nil {
		hc = &http.Client{Timeout: smscap.HTTPTimeout}
	}
	return smscap.New(smscap.Options{
		Base:    r.base,
		Token:   r.token,
		Keyword: r.keyword,
		HTTP:    hc,
		Proxy:   r.proxy,
	})
}

var _ core.SMSProvider = (*Client)(nil)

func (c *Client) SMSStatus(ctx context.Context, opts core.SMSOpts) core.SMSStatus {
	r := c.resolveSMS(opts)
	st := core.SMSStatus{
		Configured: r.token != "",
		Provider:   traeSMSProvider,
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
	ctx, cancel := context.WithTimeout(ctxOrBackground(ctx), smscap.HTTPTimeout)
	defer cancel()
	if balance, err := sc.Balance(ctx); err == nil {
		st.Balance = strings.TrimSpace(balance)
	} else {
		st.Error = core.Redact(err.Error())
	}
	return st
}

func (c *Client) AcquirePhone(ctx context.Context, opts core.SMSOpts, want string, avoid []string) (core.SMSNumber, error) {
	r := c.resolveSMS(opts)
	sc, err := c.newSMSClient(r)
	if err != nil {
		return core.SMSNumber{}, err
	}
	ctx, cancel := context.WithTimeout(ctxOrBackground(ctx), smscap.HTTPTimeout*2)
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
	for attempt := 0; attempt < traeDupRetries; attempt++ {
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
		// A number already in the batch is not useful; hand it straight back
		// before drawing the next one so it does not stay billed to us.
		_ = sc.Release(ctx, num)
	}
	return core.SMSNumber{}, fmt.Errorf("the platform kept returning numbers already in use; try another province or card type")
}

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
	ctx, cancel := context.WithTimeout(ctxOrBackground(ctx), smscap.HTTPTimeout)
	defer cancel()
	msg, err := sc.GetMsg(ctx, phone)
	if err != nil {
		return core.SMSCode{}, err
	}
	if strings.TrimSpace(msg) == "" {
		return core.SMSCode{Ready: false}, nil
	}
	code := smscap.ExtractCode(msg)
	return core.SMSCode{Ready: code != "", Code: code, Raw: core.Redact(msg)}, nil
}

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
	ctx, cancel := context.WithTimeout(ctxOrBackground(ctx), smscap.HTTPTimeout)
	defer cancel()
	if block {
		return sc.Block(ctx, phone)
	}
	return sc.Release(ctx, phone)
}

// ctxOrBackground guards against a nil context.
func ctxOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
