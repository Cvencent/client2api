package openaicompat

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"client2api/internal/core"
	"client2api/internal/smscap"
)

// officeace_sms.go implements the one-time-SMS capability for the built-in
// Huawei OfficeAce platform.  The Huawei account page can sign in with a phone
// number, so the panel can rent a number from the shared eomsg service, read the
// verification code, and let the auto-login driver submit it.
//
// The platform token is deliberately not stored in core.SourceConfig: the panel
// sends it per request, and EOMSG_TOKEN remains the process-level fallback.  A
// missing token means the capability is visible but reports "not configured"
// instead of making an unauthenticated request.

const (
	officeAceSMSProvider     = "eomsg"
	officeAceDefaultSMSKey   = "华为"
	officeAceDefaultCardType = "全部"
	officeAceDupRetries      = 6
)

type officeAceSMSResolved struct {
	token     string
	keyword   string
	base      string
	proxy     string
	provinces []string
	cardType  string
}

// resolveSMS folds the request override and the process environment into one
// effective set of platform settings.  There is intentionally no module config
// for this platform: the built-in source has a fixed platform ID, and the
// panel's per-request token is the only credential the operator needs to paste.
func (o *officeAceSource) resolveSMS(opts core.SMSOpts) officeAceSMSResolved {
	r := officeAceSMSResolved{}
	if o != nil {
		r.base = strings.TrimSpace(o.smsBase)
	}
	r.token = strings.TrimSpace(opts.Token)
	if r.token == "" {
		r.token = strings.TrimSpace(os.Getenv("EOMSG_TOKEN"))
	}
	r.keyword = strings.TrimSpace(opts.Keyword)
	if r.keyword == "" {
		r.keyword = officeAceDefaultSMSKey
	}
	r.proxy = strings.TrimSpace(opts.Proxy)
	r.cardType = strings.TrimSpace(opts.CardType)
	if r.cardType == "" {
		r.cardType = officeAceDefaultCardType
	}
	if r.base == "" {
		r.base = smscap.DefaultBase
	}
	r.provinces = smscap.CleanProvinces(append([]string(nil), smscap.Provinces...))
	return r
}

func (o *officeAceSource) newSMSClient(r officeAceSMSResolved) (*smscap.Client, error) {
	if strings.TrimSpace(r.token) == "" {
		return nil, fmt.Errorf("no SMS platform token: set EOMSG_TOKEN or paste one in the panel")
	}
	var hc *http.Client
	if o != nil && o.source != nil {
		hc = o.source.deps.HTTPClient
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

var _ core.SMSProvider = (*officeAceSource)(nil)

func (o *officeAceSource) SMSStatus(ctx context.Context, opts core.SMSOpts) core.SMSStatus {
	r := o.resolveSMS(opts)
	st := core.SMSStatus{
		Configured: r.token != "",
		Provider:   officeAceSMSProvider,
		Keyword:    r.keyword,
		Provinces:  r.provinces,
		CardTypes:  smscap.CardTypes,
	}
	if !st.Configured {
		return st
	}
	sc, err := o.newSMSClient(r)
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

func (o *officeAceSource) AcquirePhone(ctx context.Context, opts core.SMSOpts, want string, avoid []string) (core.SMSNumber, error) {
	r := o.resolveSMS(opts)
	sc, err := o.newSMSClient(r)
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
	for attempt := 0; attempt < officeAceDupRetries; attempt++ {
		prov := province
		if prov == "" {
			prov = o.smsRot.Next(r.provinces)
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

func (o *officeAceSource) PollSMSCode(ctx context.Context, opts core.SMSOpts, phone string) (core.SMSCode, error) {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return core.SMSCode{}, fmt.Errorf("a phone number is required")
	}
	r := o.resolveSMS(opts)
	sc, err := o.newSMSClient(r)
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

func (o *officeAceSource) ReleasePhone(ctx context.Context, opts core.SMSOpts, phone string, block bool) error {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return fmt.Errorf("a phone number is required")
	}
	r := o.resolveSMS(opts)
	sc, err := o.newSMSClient(r)
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
