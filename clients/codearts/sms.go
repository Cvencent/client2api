package codearts

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"client2api/internal/core"
	"client2api/internal/smscap"
)

// sms.go implements the one-time-SMS capability for the codearts module, the
// same panel experience the officeace platform already has.  The Huawei
// sign-in page behind the CodeArts portal can log in with a phone number, so
// the panel rents a number from the shared eomsg service, reads the code, and
// the auto-login driver submits it.
//
// The platform token is deliberately not stored in config: the panel sends it
// per request and EOMSG_TOKEN remains the process-level fallback.  A missing
// token means the capability is visible but reports "not configured" instead
// of making an unauthenticated request.

const (
	codeartsSMSProvider     = "eomsg"
	codeartsDefaultSMSKey   = "华为"
	codeartsDefaultCardType = "全部"
	codeartsDupRetries      = 6
)

// ctxOrBackground guards against a nil context, the way every other
// long-running helper in the package does.
func ctxOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

type codeartsSMSResolved struct {
	token     string
	keyword   string
	base      string
	proxy     string
	provinces []string
	cardType  string
}

// resolveSMS folds the request override and the process environment into one
// effective set of platform settings.
func (c *Client) resolveSMS(opts core.SMSOpts) codeartsSMSResolved {
	r := codeartsSMSResolved{}
	r.token = strings.TrimSpace(opts.Token)
	if r.token == "" {
		r.token = strings.TrimSpace(os.Getenv("EOMSG_TOKEN"))
	}
	r.keyword = strings.TrimSpace(opts.Keyword)
	if r.keyword == "" {
		r.keyword = codeartsDefaultSMSKey
	}
	r.proxy = strings.TrimSpace(opts.Proxy)
	r.cardType = strings.TrimSpace(opts.CardType)
	if r.cardType == "" {
		r.cardType = codeartsDefaultCardType
	}
	r.base = smscap.DefaultBase
	r.provinces = smscap.CleanProvinces(append([]string(nil), smscap.Provinces...))
	return r
}

func (c *Client) newSMSClient(r codeartsSMSResolved) (*smscap.Client, error) {
	if strings.TrimSpace(r.token) == "" {
		return nil, fmt.Errorf("no SMS platform token: set EOMSG_TOKEN or paste one in the panel")
	}
	var hc *http.Client
	if c != nil {
		hc = c.httpClient()
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
		Provider:   codeartsSMSProvider,
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
	for attempt := 0; attempt < codeartsDupRetries; attempt++ {
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
