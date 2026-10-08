package workbuddy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"client2api/internal/core"
)

// sms.go is the module's optional "rent a phone number from a one-time-SMS
// platform" capability (core.SMSProvider).
//
// The flow it serves is the one wb-auto used to drive a headless browser for:
//
//	1. ask the panel for an authorisation URL (weblogin.go's StartLoginRealm);
//	2. rent a number here, and let the operator type it into the vendor's page;
//	3. poll the platform for the code the vendor texted;
//	4. let the operator paste the code in, then release the number.
//
// The browser step stays with the operator because the vendor's login page is a
// cross-origin SPA that no stdlib-Go program can script.  Everything else --
// number acquisition, the pool-duplicate guard, province rotation, code
// extraction, release/blacklist -- is ported from wb-auto/wb_add_account.py and
// wb-auto/wb_restore_account.py, which both talked to eomsg.
//
// The platform protocol is a plain GET API (https://api.eomsg.com/zc/data.php):
//
//	code=leftAmount   -> balance
//	code=getPhone     -> a number, or "ERROR…"
//	code=getMsg       -> the SMS text, or "[尚未收到]" while it has not arrived
//	code=release      -> hand the number back
//	code=block        -> blacklist the number
//
// Every reply is text, and a reply that starts with "ERROR" is a failure.  The
// token is the operator's platform credential; it is never logged and never
// echoed back to the panel.

const (
	// defaultSMSBase is the eomsg endpoint.  wb-auto hard-coded the same URL.
	defaultSMSBase = "https://api.eomsg.com/zc/data.php"
	// defaultSMSKeyword is the sender keyword the CodeBuddy SMS carries.  The
	// platform filters on it, so a wrong keyword reads as "no message".
	defaultSMSKeyword = "腾讯科技"
	// smsHTTPTimeout bounds one platform call.  wb-auto used 25s.
	smsHTTPTimeout = 25 * time.Second
	// smsMaxBody bounds what we are willing to read from the platform.
	smsMaxBody = 64 << 10
)

// smsProvinces is the built-in rotation pool: every province-level region, so
// consecutive rents do not cluster in one city.  The platform silently ignores
// a name it does not know, so an unfamiliar name is harmless.
var smsProvinces = []string{
	"北京", "天津", "上海", "重庆",
	"河北", "山西", "辽宁", "吉林", "黑龙江", "江苏", "浙江", "安徽",
	"福建", "江西", "山东", "河南", "湖北", "湖南", "广东", "海南",
	"四川", "贵州", "云南", "陕西", "甘肃", "青海",
	"内蒙古", "广西", "西藏", "宁夏", "新疆",
}

// smsCardTypes are the classes the platform accepts (wb-auto's --sms-card-type).
var smsCardTypes = []string{"实卡", "虚卡", "全部"}

var (
	// smsCodeCtxRe prefers the digits that sit next to a "code" word.
	smsCodeCtxRe = regexp.MustCompile(`(?i)(?:验证码|校验码|动态码|verification\s*code|code)[^\d]{0,12}(\d{4,8})\b`)
	// smsCodeAnyRe is the fallback: any standalone 4-8 digit run.
	smsCodeAnyRe = regexp.MustCompile(`(?:\D|^)(\d{4,8})(?:\D|$)`)
	// smsPhoneRe validates what the platform returned as a number.
	smsPhoneRe = regexp.MustCompile(`^\d{6,15}$`)
)

// extractSMSCode pulls the verification code out of an SMS body.  It is the
// same two-step match as wb-auto's extract_code.
func extractSMSCode(message string) string {
	if m := smsCodeCtxRe.FindStringSubmatch(message); m != nil {
		return m[1]
	}
	if m := smsCodeAnyRe.FindStringSubmatch(message); m != nil {
		return m[1]
	}
	return ""
}

// smsClient is one configured platform client.  It is built per call from the
// module's config plus whatever the operator overrode, so a pasted token takes
// effect without a restart.
type smsClient struct {
	base    string
	token   string
	keyword string
	http    *http.Client
}

// call performs one GET and returns the raw text body.  A body starting with
// "ERROR" becomes an error; the token is never included in any error text.
func (s *smsClient) call(ctx context.Context, params url.Values) (string, error) {
	params.Set("token", s.token)
	endpoint := s.base + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("build the platform request: %w", err)
	}
	req.Header.Set("Accept", "*/*")
	resp, err := s.http.Do(req)
	if err != nil {
		// A *url.Error prints the whole request URL, and the token lives in
		// its query string, so the message is scrubbed before it can become a
		// panel error.
		if errors.Is(err, context.DeadlineExceeded) {
			return "", fmt.Errorf("SMS platform request timed out: %w", context.DeadlineExceeded)
		}
		if errors.Is(err, context.Canceled) {
			return "", fmt.Errorf("SMS platform request cancelled: %w", context.Canceled)
		}
		return "", fmt.Errorf("cannot reach the SMS platform: %s", s.scrub(err.Error(), endpoint))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, smsMaxBody))
	if err != nil {
		return "", fmt.Errorf("cannot read the platform response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("the SMS platform returned HTTP %d", resp.StatusCode)
	}
	body := strings.TrimSpace(string(raw))
	if strings.HasPrefix(body, "ERROR") {
		msg := strings.TrimSpace(strings.TrimPrefix(body, "ERROR"))
		if msg == "" {
			msg = "the platform refused the request"
		}
		return "", fmt.Errorf("the SMS platform refused the request: %s", s.scrub(msg, endpoint))
	}
	return body, nil
}

// scrub removes the credential from a platform message.  Two things can carry
// it: the token itself, and the request URL a transport error prints -- the
// token rides in that query string.  Both are replaced before the text can
// become a panel error.
func (s *smsClient) scrub(msg, endpoint string) string {
	if endpoint != "" {
		msg = strings.ReplaceAll(msg, endpoint, s.base+"?<redacted>")
	}
	if s.token != "" {
		msg = strings.ReplaceAll(msg, s.token, "<redacted>")
	}
	return msg
}

func (s *smsClient) balance(ctx context.Context) (string, error) {
	return s.call(ctx, url.Values{"code": {"leftAmount"}})
}

func (s *smsClient) getPhone(ctx context.Context, phone, province, cardType string) (string, error) {
	params := url.Values{
		"code":     {"getPhone"},
		"keyWord":  {s.keyword},
		"cardType": {cardType},
	}
	if phone != "" {
		params.Set("phone", phone)
	}
	if province != "" {
		params.Set("province", province)
	}
	body, err := s.call(ctx, params)
	if err != nil {
		return "", err
	}
	num := strings.TrimSpace(body)
	if !smsPhoneRe.MatchString(num) {
		return "", fmt.Errorf("the platform did not return a phone number (%q)", truncateForMessage(num))
	}
	return num, nil
}

// getMsg returns the SMS text, or "" while the platform says nothing has
// arrived yet.  "Nothing yet" is a waiting state, not an error.
func (s *smsClient) getMsg(ctx context.Context, phone string) (string, error) {
	body, err := s.call(ctx, url.Values{
		"code":    {"getMsg"},
		"phone":   {phone},
		"keyWord": {s.keyword},
	})
	if err != nil {
		return "", err
	}
	if strings.Contains(body, "尚未收到") {
		return "", nil
	}
	return body, nil
}

func (s *smsClient) release(ctx context.Context, phone string) error {
	_, err := s.call(ctx, url.Values{"code": {"release"}, "phone": {phone}})
	return err
}

func (s *smsClient) block(ctx context.Context, phone string) error {
	_, err := s.call(ctx, url.Values{"code": {"block"}, "phone": {phone}})
	return err
}

// ---------------------------------------------------------------------------
// Client-side plumbing
// ---------------------------------------------------------------------------

// smsResolved is one call's effective platform settings.
type smsResolved struct {
	token     string
	keyword   string
	base      string
	provinces []string
}

// resolveSMS folds the module config, the process environment and the per-call
// override into one effective setting.  Precedence is override > config > env,
// which is what an operator expects: pasting a token in the panel beats the
// file, and the file beats a stale environment variable.
func (c *Client) resolveSMS(opts core.SMSOpts) smsResolved {
	r := smsResolved{
		token:   strings.TrimSpace(c.cfg.SMSToken),
		keyword: strings.TrimSpace(c.cfg.SMSKeyword),
		base:    strings.TrimSpace(c.cfg.SMSBase),
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
		r.base = defaultSMSBase
	}
	switch {
	case len(c.cfg.SMSProvinces) > 0:
		r.provinces = append([]string(nil), c.cfg.SMSProvinces...)
	default:
		r.provinces = append([]string(nil), smsProvinces...)
	}
	// The escape hatch: an operator who wants the platform to choose the
	// province writes "none" (or leaves a single empty entry) instead of
	// having to enumerate all 31.
	clean := r.provinces[:0]
	for _, p := range r.provinces {
		p = strings.TrimSpace(p)
		if p == "" || strings.EqualFold(p, "none") || p == "不限" {
			continue
		}
		clean = append(clean, p)
	}
	r.provinces = clean
	return r
}

// newSMSClient builds the platform client for one call.  It refuses to build
// one without a token, which is what turns "not configured" into a clear error
// instead of an unauthenticated request.
func (c *Client) newSMSClient(r smsResolved) (*smsClient, error) {
	if r.token == "" {
		return nil, fmt.Errorf("no SMS platform token: set clients.workbuddy.sms_token (or EOMSG_TOKEN), or paste one in the panel")
	}
	hc := c.deps.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: smsHTTPTimeout}
	}
	return &smsClient{base: r.base, token: r.token, keyword: r.keyword, http: hc}, nil
}

// nextProvince hands out the next province in this round's rotation.  The pool
// is reshuffled when it runs out, excluding the province just used, so two
// consecutive rents never land in the same province.  An empty pool means
// "let the platform choose".
func (c *Client) nextProvince(pool []string) string {
	if len(pool) == 0 {
		return ""
	}
	c.smsMu.Lock()
	defer c.smsMu.Unlock()
	if len(c.smsRotation) == 0 {
		c.smsRotation = append([]string(nil), pool...)
		rand.Shuffle(len(c.smsRotation), func(i, j int) {
			c.smsRotation[i], c.smsRotation[j] = c.smsRotation[j], c.smsRotation[i]
		})
		if c.smsLastProv != "" && len(c.smsRotation) > 1 && c.smsRotation[0] == c.smsLastProv {
			c.smsRotation[0], c.smsRotation[1] = c.smsRotation[1], c.smsRotation[0]
		}
	}
	prov := c.smsRotation[0]
	c.smsRotation = c.smsRotation[1:]
	c.smsLastProv = prov
	return prov
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
		CardTypes:  smsCardTypes,
	}
	if !st.Configured {
		return st
	}
	sc, err := c.newSMSClient(r)
	if err != nil {
		st.Error = core.Redact(err.Error())
		return st
	}
	ctx, cancel := context.WithTimeout(ctx, smsHTTPTimeout)
	defer cancel()
	if bal, err := sc.balance(ctx); err == nil {
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
	cardType := strings.TrimSpace(opts.CardType)
	if cardType == "" {
		cardType = "全部"
	}
	ctx, cancel := context.WithTimeout(ctx, smsHTTPTimeout*2)
	defer cancel()

	if want := strings.TrimSpace(want); want != "" {
		num, err := sc.getPhone(ctx, want, strings.TrimSpace(opts.Province), cardType)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return core.SMSNumber{}, fmt.Errorf("重新占用号码超时：%w", err)
			}
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
			prov = c.nextProvince(r.provinces)
		}
		num, err := sc.getPhone(ctx, "", prov, cardType)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return core.SMSNumber{}, fmt.Errorf("取号超时：%w", err)
			}
			return core.SMSNumber{}, err
		}
		if !skip[num] {
			return core.SMSNumber{Phone: num, Province: prov, Keyword: r.keyword}, nil
		}
		// Already in the caller's set: give it straight back so the platform
		// does not bill us for a number we will not use, then draw again.
		_ = sc.release(ctx, num)
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
	ctx, cancel := context.WithTimeout(ctx, smsHTTPTimeout)
	defer cancel()
	msg, err := sc.getMsg(ctx, phone)
	if err != nil {
		return core.SMSCode{}, err
	}
	if msg == "" {
		return core.SMSCode{Ready: false}, nil
	}
	code := extractSMSCode(msg)
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
	ctx, cancel := context.WithTimeout(ctx, smsHTTPTimeout)
	defer cancel()
	if block {
		return sc.block(ctx, phone)
	}
	return sc.release(ctx, phone)
}

// truncateForMessage keeps an upstream string short enough for a panel toast
// without letting a huge body through.
func truncateForMessage(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}
