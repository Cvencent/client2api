// Package smscap is the shared client for the one-time-SMS platform the panel
// rents phone numbers from (eomsg).
//
// Two modules need it: workbuddy, whose vendor login is a browser flow that
// needs a number the operator can type in, and loomy, whose vendor login is a
// plain HTTP phone-code API the module can drive end to end.  The platform
// protocol is identical for both, so it lives here once instead of twice.
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
package smscap

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultBase is the eomsg endpoint.  wb-auto hard-coded the same URL.
	DefaultBase = "https://api.eomsg.com/zc/data.php"
	// DefaultKeyword is the sender keyword a CodeBuddy SMS carries.  The
	// platform filters on it, so a wrong keyword reads as "no message".
	DefaultKeyword = "腾讯科技"
	// HTTPTimeout bounds one platform call.  wb-auto used 25s.
	HTTPTimeout = 25 * time.Second
	// MaxBody bounds what we are willing to read from the platform.
	MaxBody = 64 << 10
)

// Provinces is the built-in rotation pool: every province-level region, so
// consecutive rents do not cluster in one city.  The platform silently ignores
// a name it does not know, so an unfamiliar name is harmless.
var Provinces = []string{
	"北京", "天津", "上海", "重庆",
	"河北", "山西", "辽宁", "吉林", "黑龙江", "江苏", "浙江", "安徽",
	"福建", "江西", "山东", "河南", "湖北", "湖南", "广东", "海南",
	"四川", "贵州", "云南", "陕西", "甘肃", "青海",
	"内蒙古", "广西", "西藏", "宁夏", "新疆",
}

// CardTypes are the classes the platform accepts (wb-auto's --sms-card-type).
var CardTypes = []string{"实卡", "虚卡", "全部"}

var (
	// codeCtxRe prefers the digits that sit next to a "code" word.
	codeCtxRe = regexp.MustCompile(`(?i)(?:验证码|校验码|动态码|verification\s*code|code)[^\d]{0,12}(\d{4,8})\b`)
	// codeAnyRe is the fallback: any standalone 4-8 digit run.
	codeAnyRe = regexp.MustCompile(`(?:\D|^)(\d{4,8})(?:\D|$)`)
	// phoneRe validates what the platform returned as a number.
	phoneRe = regexp.MustCompile(`^\d{6,15}$`)
)

// ExtractCode pulls the verification code out of an SMS body.  It is the same
// two-step match as wb-auto's extract_code.
func ExtractCode(message string) string {
	if m := codeCtxRe.FindStringSubmatch(message); m != nil {
		return m[1]
	}
	if m := codeAnyRe.FindStringSubmatch(message); m != nil {
		return m[1]
	}
	return ""
}

// Options is one platform client's settings.  Token is required; Base and
// Keyword fall back to the defaults.
type Options struct {
	Base    string
	Token   string
	Keyword string
	HTTP    *http.Client
}

// Client is one configured platform client.  It is built per call from the
// module's config plus whatever the operator overrode, so a pasted token takes
// effect without a restart.
type Client struct {
	base    string
	token   string
	keyword string
	http    *http.Client
}

// New builds a platform client.  A missing token is an error rather than an
// unauthenticated request.
func New(opts Options) (*Client, error) {
	token := strings.TrimSpace(opts.Token)
	if token == "" {
		return nil, fmt.Errorf("no one-time-SMS platform token is configured")
	}
	base := strings.TrimSpace(opts.Base)
	if base == "" {
		base = DefaultBase
	}
	keyword := strings.TrimSpace(opts.Keyword)
	if keyword == "" {
		keyword = DefaultKeyword
	}
	hc := opts.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: HTTPTimeout}
	}
	return &Client{base: base, token: token, keyword: keyword, http: hc}, nil
}

// Keyword is the sender filter this client was built with.
func (c *Client) Keyword() string { return c.keyword }

// call performs one GET and returns the raw text body.  A body starting with
// "ERROR" becomes an error; the token is never included in any error text.
func (c *Client) call(ctx context.Context, params url.Values) (string, error) {
	params.Set("token", c.token)
	endpoint := c.base + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("build the platform request: %w", err)
	}
	req.Header.Set("Accept", "*/*")
	resp, err := c.http.Do(req)
	if err != nil {
		// A *url.Error prints the whole request URL, and the token lives in
		// its query string, so the message is scrubbed before it can become a
		// panel error.
		return "", fmt.Errorf("cannot reach the SMS platform: %s", c.scrub(err.Error(), endpoint))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
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
		return "", fmt.Errorf("the SMS platform refused the request: %s", c.scrub(msg, endpoint))
	}
	return body, nil
}

// scrub removes the credential from a platform message.  Two things can carry
// it: the token itself, and the request URL a transport error prints -- the
// token rides in that query string.  Both are replaced before the text can
// become a panel error.
func (c *Client) scrub(msg, endpoint string) string {
	if endpoint != "" {
		msg = strings.ReplaceAll(msg, endpoint, c.base+"?<redacted>")
	}
	if c.token != "" {
		msg = strings.ReplaceAll(msg, c.token, "<redacted>")
	}
	return msg
}

// Balance reads the account's remaining platform credit.
func (c *Client) Balance(ctx context.Context) (string, error) {
	return c.call(ctx, url.Values{"code": {"leftAmount"}})
}

// GetPhone rents a number.  An empty phone draws a new one; a non-empty phone
// asks the platform to re-issue that exact number (the restore path).  An
// empty province or cardType lets the platform choose.
func (c *Client) GetPhone(ctx context.Context, phone, province, cardType string) (string, error) {
	params := url.Values{
		"code":     {"getPhone"},
		"keyWord":  {c.keyword},
		"cardType": {cardType},
	}
	if phone != "" {
		params.Set("phone", phone)
	}
	if province != "" {
		params.Set("province", province)
	}
	body, err := c.call(ctx, params)
	if err != nil {
		return "", err
	}
	num := strings.TrimSpace(body)
	if !phoneRe.MatchString(num) {
		return "", fmt.Errorf("the platform did not return a phone number (%q)", Truncate(num))
	}
	return num, nil
}

// GetMsg returns the SMS text, or "" while the platform says nothing has
// arrived yet.  "Nothing yet" is a waiting state, not an error.
func (c *Client) GetMsg(ctx context.Context, phone string) (string, error) {
	body, err := c.call(ctx, url.Values{
		"code":    {"getMsg"},
		"phone":   {phone},
		"keyWord": {c.keyword},
	})
	if err != nil {
		return "", err
	}
	if strings.Contains(body, "尚未收到") {
		return "", nil
	}
	return body, nil
}

// Release hands the number back to the platform.
func (c *Client) Release(ctx context.Context, phone string) error {
	_, err := c.call(ctx, url.Values{"code": {"release"}, "phone": {phone}})
	return err
}

// Block blacklists the number so it is never drawn again.
func (c *Client) Block(ctx context.Context, phone string) error {
	_, err := c.call(ctx, url.Values{"code": {"block"}, "phone": {phone}})
	return err
}

// Truncate keeps an upstream string short enough for a panel toast without
// letting a huge body through.
func Truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

// CleanProvinces removes the escape-hatch entries ("none" / "不限") an operator
// uses to ask the platform to choose, and trims the rest.  An empty result
// means "let the platform choose".
func CleanProvinces(pool []string) []string {
	out := make([]string, 0, len(pool))
	for _, p := range pool {
		p = strings.TrimSpace(p)
		if p == "" || strings.EqualFold(p, "none") || p == "不限" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Rotator hands out the next province in a shuffled round, excluding the one it
// just used so two consecutive rents never land in the same province.  An empty
// pool means "let the platform choose".  It is safe for concurrent use.
type Rotator struct {
	mu   sync.Mutex
	pool []string
	last string
}

// Next returns the next province, or "" when the pool is empty.
func (r *Rotator) Next(pool []string) string {
	if len(pool) == 0 {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pool) == 0 {
		r.pool = append([]string(nil), pool...)
		rand.Shuffle(len(r.pool), func(i, j int) { r.pool[i], r.pool[j] = r.pool[j], r.pool[i] })
		if r.last != "" && len(r.pool) > 1 && r.pool[0] == r.last {
			r.pool[0], r.pool[1] = r.pool[1], r.pool[0]
		}
	}
	prov := r.pool[0]
	r.pool = r.pool[1:]
	r.last = prov
	return prov
}
