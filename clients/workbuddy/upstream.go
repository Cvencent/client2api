package workbuddy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// Upstream transport and error classification.  Ported from the reference
// implementation (internal/upstream/client.go, transport.go; MIT) with the
// wiring rewritten to this module's types.

// --- transport hardening ---------------------------------------------------

const (
	dialTimeout         = 10 * time.Second
	dialKeepAlive       = 15 * time.Second
	tlsHandshakeTimeout = 10 * time.Second
	idleConnTimeout     = 30 * time.Second
	// responseHeaderTimeout is the constructor default for the chat first-byte
	// deadline.  It is deliberately the reference's transport-level safety net
	// rather than its configured value: the reference normalises a missing
	// `header_timeout_seconds` to `timeout_seconds` (default 120s), so the
	// number that actually governs a configured deployment is the one New
	// resolves from the config file.  This one covers a naked Upstream built by
	// a test or by a caller that never wired the config.
	responseHeaderTimeout = 60 * time.Second
	maxIdleConns          = 100
	maxIdleConnsPerHost   = 20
	// defaultHTTPTimeout is the reference's `timeout_seconds`: the ceiling on a
	// short RPC (token refresh, check-in, balance, FetchModels).  The chat path
	// uses ChatHTTP, whose Timeout is deliberately zero so a long stream is
	// bounded by the idle monitor instead.
	defaultHTTPTimeout = 120 * time.Second
	// defaultIdleTimeout is the reference's idle fallback (its normalize() lifts
	// an unset `idle_timeout_seconds` to 300s).
	defaultIdleTimeout = 300 * time.Second
)

func newDialer() *net.Dialer {
	return &net.Dialer{Timeout: dialTimeout, KeepAlive: dialKeepAlive}
}

// newTransport returns a hardened transport with HTTP/2 disabled.  An empty
// (non-nil) TLSNextProto map is the only reliable way to do this: with a custom
// DialContext, ForceAttemptHTTP2=false still negotiates h2 through ALPN and
// produces "http2: timeout awaiting response headers".
//
// headerTimeout is the chat first-byte deadline.  A non-positive value keeps the
// built-in default, so a caller with no configuration still gets the hardened
// transport.
func newTransport(headerTimeout time.Duration) *http.Transport {
	if headerTimeout <= 0 {
		headerTimeout = responseHeaderTimeout
	}
	d := newDialer()
	return &http.Transport{
		DialContext:           d.DialContext,
		TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper),
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		IdleConnTimeout:       idleConnTimeout,
		ResponseHeaderTimeout: headerTimeout,
	}
}

type closeIdler interface{ CloseIdleConnections() }

// roundTripCloseIdle drops pooled connections after a transport-level failure;
// otherwise a dead connection lingers until IdleConnTimeout and gets reused.
func roundTripCloseIdle(rt http.RoundTripper) {
	if rt == nil {
		return
	}
	if ci, ok := rt.(closeIdler); ok {
		ci.CloseIdleConnections()
	}
}

// idleBody cancels the request context when the upstream stops sending data, so
// a stalled stream cannot pin a goroutine forever.  The first byte is already
// covered by the transport's ResponseHeaderTimeout.
type idleBody struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
	idle   time.Duration
	mu     sync.Mutex
	timer  *time.Timer
	closed bool
}

func monitorBody(rc io.ReadCloser, idle time.Duration, cancel context.CancelFunc) io.ReadCloser {
	if rc == nil || cancel == nil {
		return rc
	}
	b := &idleBody{rc: rc, cancel: cancel, idle: idle}
	if idle > 0 {
		// Publish the timer under b.mu.  fire() runs on its own goroutine and
		// reads b.timer under this same lock, so an unsynchronised assignment
		// here races with the very first fire (which a short idle reaches
		// immediately).
		b.mu.Lock()
		b.timer = time.AfterFunc(idle, func() { b.fire() })
		b.mu.Unlock()
	}
	return b
}

func (b *idleBody) fire() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()
	// Cancelling the request context unblocks a pending read on a real
	// http.Response.Body; closing the body as well covers transports that
	// ignore context cancellation, so a stalled stream can never pin a
	// goroutine forever.
	if b.cancel != nil {
		b.cancel()
	}
	_ = b.rc.Close()
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.mu.Lock()
	if b.idle > 0 && !b.closed && b.timer != nil {
		b.timer.Reset(b.idle)
	}
	b.mu.Unlock()
	return n, err
}

func (b *idleBody) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()
	err := b.rc.Close()
	if b.cancel != nil {
		b.cancel()
	}
	return err
}

// --- error classification --------------------------------------------------

// ErrKind classifies an upstream failure.  The pool uses it to decide between
// "refresh and retry", "cool down briefly" and "park the account".
type ErrKind int

const (
	ErrNone ErrKind = iota
	ErrHardCredit
	ErrSoftRate
	ErrSessionDead
	ErrNotFound
	ErrServer
	ErrContentBlocked
	ErrBadParams
	ErrAccountFault
	ErrModelBlocked
	ErrWafBlock
	ErrPromptTooLong
	ErrImageInvalid
	ErrClient
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrContentBlocked:
		return "content_blocked"
	case ErrBadParams:
		return "bad_params"
	case ErrAccountFault:
		return "account_fault"
	case ErrModelBlocked:
		return "model_blocked"
	case ErrWafBlock:
		return "waf_block"
	case ErrPromptTooLong:
		return "prompt_too_long"
	case ErrImageInvalid:
		return "image_invalid"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error is a classified upstream failure.
type Error struct {
	Kind       ErrKind
	Status     int
	Msg        string
	RetryAfter time.Duration
	// ResetAt is the wall clock at which a *model-scoped* rate limit lifts, as
	// reported by the vendor's own message ("... 将在 <t> 重置" / "reset at
	// <t>").  It is the zero time when the message dated nothing.
	//
	// When set, the pool parks the model until this instant on this account,
	// capped at its own softModelMax, instead of inventing a backoff.  The
	// vendor knows when its own window closes; guessing it is guesswork.
	ResetAt time.Time
	// ModelScoped marks a failure that belongs to (account, model) rather than
	// to the account.  The vendor's 6004 rate limit is the one such answer: the
	// account is otherwise healthy and can serve every other model, so parking
	// the credential would drop a good account for a request-level problem.
	//
	// It never changes Kind.  A 6004 stays ErrSoftRate for every existing
	// caller and every existing test; this flag only tells the pool how far the
	// cooldown reaches.
	ModelScoped bool
}

func (e *Error) Error() string {
	if e == nil {
		return "upstream error"
	}
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

var (
	hardMarkers = []string{
		"insufficient credit", "no credit", "credit exhausted", "credits exhausted",
		"out of credit", "quota exceeded", "quota exhaust", "payment required",
		"credit not enough", "not enough credit",
		"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
	}
	softRateMarkers = []string{
		"rate limit", "rate-limiting", "rate-limited", "too many requests",
		"too many", "usage limit", "请求过于频繁", "限流",
	}
	sessionDeadMarkers = []string{
		"Offline user session not found", "12153",
	}
	accountFaultMarkers = []string{
		"request illegal", "trial not activated", "trial version is not yet activated",
	}
	contentBlockedMarkers = []string{
		"blocked by security policy", "unapproved channel", "illegal api invocation",
	}
	invalidImageMarkers = []string{
		"invalid image_url content", "invalid_image_data", "replace the image",
	}
	promptTooLongMarkers = []string{
		`"code":11115`, `"code": 11115`, `"code":"11115"`, "prompt is too long",
	}
)

const (
	badParamsMarkerMsg  = "Unmarshal chat params failed"
	badParamsMarkerCode = `"code":11101`
	modelBlockCode      = "11102"
	modelBlockMsgMarker = "service info not found"
	// ModelBlockReason is the operator-facing explanation for code 11102.
	ModelBlockReason = "11102 model not available"
)

var (
	softRateResetLoc      = time.FixedZone("UTC+8", 8*60*60)
	softRateTimeLayout    = "2006-01-02 15:04:05"
	reModelRateLimit      = regexp.MustCompile(`"code"\s*:\s*"?6004"?`)
	reSoftRateResetCN     = regexp.MustCompile(`将在 (.+?) 重置`)
	reSoftRateResetEN     = regexp.MustCompile(`(?i)reset at (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`)
	reBusinessCode11135   = regexp.MustCompile(`"code"\s*:\s*"?11135"?`)
	reRetryAfterCandidate = regexp.MustCompile(`^\d+$`)
)

// SoftRateResetLoc exposes the fixed UTC+8 zone used by the CN rate-limit reset
// messages.
func SoftRateResetLoc() *time.Location { return softRateResetLoc }

// IsModelRateLimit reports the per-model rate-limit business code (6004).
func IsModelRateLimit(body string) bool { return reModelRateLimit.MatchString(body) }

func isPromptTooLongStatus(status int) bool {
	return status == 400 || status == 404 || status == 413
}

// IsModelBlocked detects business code 11102 ("service info not found"), which
// means this account cannot use this model at all.  It is only meaningful on
// 400/404.
func IsModelBlocked(status int, body string) bool {
	if status != 400 && status != 404 {
		return false
	}
	if body == "" {
		return false
	}
	if !strings.Contains(body, modelBlockCode) && !strings.Contains(strings.ToLower(body), modelBlockMsgMarker) {
		return false
	}
	var root map[string]any
	if json.Unmarshal([]byte(body), &root) != nil || root == nil {
		return false
	}
	nodes := []map[string]any{root}
	if e, ok := root["error"].(map[string]any); ok && e != nil {
		nodes = append(nodes, e)
	}
	code := ""
	msg := ""
	for _, n := range nodes {
		for _, k := range []string{"code", "errCode", "error_code"} {
			if v, ok := n[k]; ok && v != nil {
				code = strings.TrimSpace(fmt.Sprint(v))
				break
			}
		}
		for _, k := range []string{"msg", "message"} {
			if s, ok := n[k].(string); ok && s != "" {
				msg = s
				break
			}
		}
	}
	if code == modelBlockCode {
		return true
	}
	return strings.Contains(strings.ToLower(msg), modelBlockMsgMarker)
}

// hasBusinessCode reports whether any node in the decoded body carries the
// given business code.
func hasBusinessCode(body, want string) bool {
	var v any
	if json.Unmarshal([]byte(body), &v) != nil {
		return false
	}
	return walkBusinessCode(v, want)
}

func walkBusinessCode(v any, want string) bool {
	switch n := v.(type) {
	case map[string]any:
		if c, ok := n["code"]; ok && c != nil {
			if strings.TrimSpace(fmt.Sprint(c)) == want {
				return true
			}
		}
		for _, val := range n {
			if walkBusinessCode(val, want) {
				return true
			}
		}
	case []any:
		for _, val := range n {
			if walkBusinessCode(val, want) {
				return true
			}
		}
	}
	return false
}

// hasBusinessEnvelope reports whether the body carries the gateway's business
// envelope keys.
func hasBusinessEnvelope(body string) bool {
	return strings.Contains(body, `"code":`) || strings.Contains(body, `"msg":`)
}

// IsWafBlocked detects a WAF rejection: 403 without a business envelope.
func IsWafBlocked(status int, body string) bool {
	return status == 403 && !hasBusinessEnvelope(body)
}

var retryAfterHeaderCandidates = []string{"Retry-After", "Retry-After-Ms", "X-Ratelimit-Reset"}

const retryAfterSanity = 2 * time.Hour

// ParseRetryAfter extracts a usable retry delay from the response headers.
func ParseRetryAfter(h http.Header) (time.Duration, bool) {
	if h == nil {
		return 0, false
	}
	for _, name := range retryAfterHeaderCandidates {
		v := strings.TrimSpace(h.Get(name))
		if v == "" || !reRetryAfterCandidate.MatchString(v) {
			continue
		}
		d, ok := parseRetryNumber(v, name)
		if !ok || d <= 0 || d > retryAfterSanity {
			continue
		}
		return d, true
	}
	return 0, false
}

// parseRetryNumber converts a header value to a duration.  X-Ratelimit-Reset is
// an absolute epoch (seconds, or milliseconds when the value is long enough).
func parseRetryNumber(v, headerName string) (time.Duration, bool) {
	if len(v) > 16 {
		return 0, false
	}
	var n int64
	for i := 0; i < len(v); i++ {
		ch := v[i]
		if ch < '0' || ch > '9' {
			return 0, false
		}
		n = n*10 + int64(ch-'0')
	}
	switch headerName {
	case "Retry-After":
		return time.Duration(n) * time.Second, true
	case "Retry-After-Ms":
		return time.Duration(n) * time.Millisecond, true
	default:
		sec := n
		if len(v) >= 12 {
			sec = n / 1000
		}
		return time.Until(time.Unix(sec, 0)), true
	}
}

// ParseRateReset extracts the absolute reset time from a rate-limit message.
func ParseRateReset(body string) (time.Time, bool) {
	m := reSoftRateResetCN.FindStringSubmatch(body)
	if m == nil {
		m = reSoftRateResetEN.FindStringSubmatch(body)
	}
	if len(m) < 2 {
		return time.Time{}, false
	}
	ts := strings.TrimSuffix(strings.TrimSpace(m[1]), " UTC+8")
	t, err := time.ParseInLocation(softRateTimeLayout, ts, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// containsAny checks the raw body and its lowercase form.
func containsAny(body, lower string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(body, m) || strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// Classify maps an HTTP status plus body to an ErrKind.  The order matters: the
// more specific signals are tested first.
func Classify(status int, body string) ErrKind {
	if IsModelBlocked(status, body) {
		return ErrModelBlocked
	}
	if status == 402 {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	if containsAny(body, lower, sessionDeadMarkers) {
		return ErrSessionDead
	}
	if containsAny(body, lower, accountFaultMarkers) {
		return ErrAccountFault
	}
	if status == 429 && hasBusinessCode(body, "14018") {
		return ErrHardCredit
	}
	if status == 429 {
		return ErrSoftRate
	}
	if containsAny(body, lower, hardMarkers) {
		return ErrHardCredit
	}
	if containsAny(body, lower, softRateMarkers) {
		return ErrSoftRate
	}
	if isPromptTooLongStatus(status) && containsAny(body, lower, promptTooLongMarkers) {
		return ErrPromptTooLong
	}
	if status == 404 {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if IsWafBlocked(status, body) {
		return ErrWafBlock
	}
	if status == 400 && reBusinessCode11135.MatchString(lower) {
		return ErrImageInvalid
	}
	if status == 400 && containsAny(body, lower, invalidImageMarkers) {
		return ErrImageInvalid
	}
	if status >= 400 && containsAny(body, lower, contentBlockedMarkers) {
		return ErrContentBlocked
	}
	// The marker check is deliberately NOT status-gated: upstream reports
	// parameter problems as HTTP 200 carrying a business envelope, and doJSON
	// classifies those by re-running Classify over the envelope message.
	if strings.Contains(body, badParamsMarkerMsg) || strings.Contains(body, badParamsMarkerCode) {
		return ErrBadParams
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

// --- client ----------------------------------------------------------------

type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

const (
	defaultGlobalBase = "https://www.workbuddy.ai"

	chatCompletionsPath = "/v2/chat/completions"

	billingMeterPath    = "/billing/meter/get-user-resource"
	dailyCheckinPath    = "/billing/meter/daily-checkin"
	billingMeterPathV2  = "/v2/billing/meter/get-user-resource"
	dailyCheckinPathV2  = "/v2/billing/meter/daily-checkin"
	refreshTokenPath    = "/v2/plugin/auth/token/refresh"
	enterpriseModelsPth = "/console/enterprises/personal/models"
	v3ConfigPath        = "/v3/config"
)

const (
	codeBuddyIDEUA = "CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0"
	codeBuddyCLIUA = "CLI/2.63.2 CodeBuddy/2.63.2"
)

const (
	refreshIOTimeout          = 30 * time.Second
	refreshTokenExpiresInMax  = 10 * 365 * 24 * time.Hour
	maxErrorBodyBytes         = 1 << 20
	modelsCacheResponseLimit  = 1 << 20
	defaultModelListPageLimit = 0
)

// Upstream talks to the WorkBuddy gateway.  It holds no account state: every
// method takes the account it should use.
type Upstream struct {
	HTTP     *http.Client
	ChatHTTP *http.Client

	IdleTimeout time.Duration

	// HeaderTimeout is the reference's `header_timeout_seconds`: how long a chat
	// attempt may wait for the first response byte before the transport gives up
	// and the gateway rotates to another account.  It is the shared transport's
	// ResponseHeaderTimeout, so it must be set before the first request — New
	// does that from the config file.  The field is exported so a caller that
	// builds an Upstream by hand can read back what was applied.
	HeaderTimeout time.Duration

	// Sanitize toggles the body scrubber.  It is written at construction and
	// again by a live reload, so read it through sanitizeOn() rather than
	// touching the field: the reload runs concurrently with in-flight requests.
	// The field stays exported because the offline tests set it directly.
	Sanitize bool

	liveMu sync.RWMutex

	UserAgent       string
	ClientVersion   string
	CliVersion      string
	ClientName      string
	PassthroughIP   bool
	DeviceToken     string
	DeviceTokenFile string

	ChatBaseCN     string
	BillingBaseCN  string
	WebBaseCN      string
	ChatBaseGlobal string
	// BillingBaseGlobal is the reference's `global.billing_base`: the
	// international realm's billing host, overridable independently of the chat
	// host.  cmd/server/main.go:157 feeds it from cfg.Global.BillingBase, and
	// internal/upstream/client.go:710 globalBillingBase() falls back to the
	// built-in default when it is empty.  The register wizard rides the same
	// host (internal/upstream/global_register.go:44).
	BillingBaseGlobal string

	GlobalEnabled bool

	Logf func(format string, args ...any)

	effortsMu      sync.RWMutex
	efforts        map[string]map[string][]string
	defaultEfforts map[string]map[string]string

	// globalModels caches the international catalogue separately: the two
	// realms serve different catalogues, so one must never answer for the other.
	globalModels globalModelsCache
}

// NewUpstream returns a client with the reference defaults.
func NewUpstream() *Upstream {
	tr := newTransport(responseHeaderTimeout)
	return &Upstream{
		HTTP:              &http.Client{Timeout: defaultHTTPTimeout, Transport: tr},
		ChatHTTP:          &http.Client{Timeout: 0, Transport: tr},
		IdleTimeout:       defaultIdleTimeout,
		HeaderTimeout:     responseHeaderTimeout,
		ChatBaseCN:        "https://copilot.tencent.com",
		BillingBaseCN:     "https://www.codebuddy.cn",
		WebBaseCN:         "https://www.workbuddy.cn",
		ChatBaseGlobal:    defaultGlobalBase,
		BillingBaseGlobal: defaultGlobalBase,
		GlobalEnabled:     true,
	}
}

// SetTimeouts applies the reference's `upstream.*_timeout_seconds` trio:
//
//   - total bounds a short RPC (token refresh, check-in, balance, FetchModels)
//     through the control-plane client's Timeout;
//   - header is the chat first-byte deadline, installed on the shared transport
//     as ResponseHeaderTimeout.  Only the wait for response headers is counted,
//     so a stream that has already started is unaffected — its silence is
//     bounded by the idle monitor instead;
//   - idle is how long a running stream may stay silent before it is cancelled.
//
// The caller passes already-resolved values: New turns the config file into them
// and applies the reference's fallbacks (header → total, idle → 300s) before
// calling, so this method does not re-derive defaults.  A non-positive total or
// header leaves that field alone; idle is applied as given, because zero is this
// module's documented "do not monitor the stream" escape hatch.
//
// It must run before the first request: the header deadline lives on the shared
// transport, and a live swap would race an in-flight call.
func (u *Upstream) SetTimeouts(total, header, idle time.Duration) {
	if u == nil {
		return
	}
	if total > 0 && u.HTTP != nil {
		u.HTTP.Timeout = total
	}
	if header > 0 {
		u.HeaderTimeout = header
		// HTTP and ChatHTTP share one transport, so the deadline is installed
		// once; the assertion is still per client in case a caller swapped in a
		// custom transport for the control plane only.
		if tr, ok := u.ChatHTTP.Transport.(*http.Transport); ok {
			tr.ResponseHeaderTimeout = header
		}
		if tr, ok := u.HTTP.Transport.(*http.Transport); ok {
			tr.ResponseHeaderTimeout = header
		}
	}
	u.IdleTimeout = idle
}

func (u *Upstream) log(format string, args ...any) {
	if u != nil && u.Logf != nil {
		u.Logf(format, args...)
	}
}

func (u *Upstream) chatHTTP() *http.Client {
	if u != nil && u.ChatHTTP != nil {
		return u.ChatHTTP
	}
	if u != nil && u.HTTP != nil {
		return u.HTTP
	}
	return http.DefaultClient
}

func (u *Upstream) globalChatBase() string {
	if u != nil && u.ChatBaseGlobal != "" {
		return u.ChatBaseGlobal
	}
	return defaultGlobalBase
}

// globalBillingBase is the international realm's billing host.  It mirrors the
// reference's globalBillingBase (internal/upstream/client.go:710): an operator
// override wins, otherwise the built-in default.  It is a separate knob from
// globalChatBase because the reference keeps them apart, and the register
// wizard follows the billing host (internal/upstream/global_register.go:44).
func (u *Upstream) globalBillingBase() string {
	if u != nil && u.BillingBaseGlobal != "" {
		return u.BillingBaseGlobal
	}
	return defaultGlobalBase
}

// globalOn reports whether the account is served by the international realm.
func (u *Upstream) globalOn(a *Auth) bool {
	return u != nil && u.GlobalEnabled && a != nil && a.IsGlobal()
}

func (u *Upstream) chatPaths(a *Auth) []string { return []string{chatCompletionsPath} }

func (u *Upstream) billingMeterPaths(a *Auth) []string {
	if u.globalOn(a) {
		return []string{billingMeterPath, billingMeterPathV2}
	}
	return []string{billingMeterPathV2}
}

func (u *Upstream) checkinMeterPaths(a *Auth) []string {
	if u.globalOn(a) {
		return []string{dailyCheckinPath, dailyCheckinPathV2}
	}
	return []string{dailyCheckinPathV2}
}

func (u *Upstream) chatBase(a *Auth) string {
	if u.globalOn(a) {
		return u.globalChatBase()
	}
	if u != nil && u.ChatBaseCN != "" {
		return u.ChatBaseCN
	}
	return "https://copilot.tencent.com"
}

func (u *Upstream) billingBase(a *Auth) string {
	if u.globalOn(a) {
		return u.globalBillingBase()
	}
	if u != nil && u.BillingBaseCN != "" {
		return u.BillingBaseCN
	}
	return "https://www.codebuddy.cn"
}

// webBase returns the vendor's own site, which the reward-claim endpoints live
// on.  The reference (internal/upstream/client.go:836-844) hard-codes the
// international site rather than following globalBillingBase, so an operator
// who moves the billing host does not move this one.
func (u *Upstream) webBase(a *Auth) string {
	if u.globalOn(a) {
		return defaultGlobalBase
	}
	if u != nil && u.WebBaseCN != "" {
		return u.WebBaseCN
	}
	return "https://www.workbuddy.cn"
}

// realmKey normalises a realm string to a cache bucket key.
func realmKey(realm string) string {
	if strings.TrimSpace(realm) == "" {
		return realmCN
	}
	return realm
}

// prepareBody applies the outbound rewrite pipeline plus the prompt-cache key.
func (u *Upstream) prepareBody(body []byte, realm, uid, conversationID string) []byte {
	efforts, defs := u.effortsSnapshot(realm), u.defaultEffortsSnapshot(realm)
	body = prepareBody(body, u.sanitizeOn(), efforts, defs)
	return InjectPromptCacheKey(body, uid, conversationID)
}

// sanitizeOn reports whether the body scrubber should run.  Nil-safe, because
// the offline tests build an Upstream by hand and call prepareBody on it.
func (u *Upstream) sanitizeOn() bool {
	if u == nil {
		return false
	}
	u.liveMu.RLock()
	defer u.liveMu.RUnlock()
	return u.Sanitize
}

// setSanitize is the live-reload writer for the scrubber switch.
func (u *Upstream) setSanitize(on bool) {
	if u == nil {
		return
	}
	u.liveMu.Lock()
	u.Sanitize = on
	u.liveMu.Unlock()
}

func (u *Upstream) effortsSnapshot(realm string) map[string][]string {
	u.effortsMu.RLock()
	defer u.effortsMu.RUnlock()
	bucket := u.efforts[realmKey(realm)]
	if len(bucket) == 0 {
		return nil
	}
	out := make(map[string][]string, len(bucket))
	for k, v := range bucket {
		out[k] = v
	}
	return out
}

func (u *Upstream) defaultEffortsSnapshot(realm string) map[string]string {
	u.effortsMu.RLock()
	defer u.effortsMu.RUnlock()
	bucket := u.defaultEfforts[realmKey(realm)]
	if len(bucket) == 0 {
		return nil
	}
	out := make(map[string]string, len(bucket))
	for k, v := range bucket {
		out[k] = v
	}
	return out
}

func (u *Upstream) storeEfforts(realm string, efforts map[string][]string, defs map[string]string) {
	if u == nil {
		return
	}
	key := realmKey(realm)
	u.effortsMu.Lock()
	defer u.effortsMu.Unlock()
	if u.efforts == nil {
		u.efforts = map[string]map[string][]string{}
	}
	if u.defaultEfforts == nil {
		u.defaultEfforts = map[string]map[string]string{}
	}
	if len(efforts) > 0 {
		u.efforts[key] = efforts
	}
	if len(defs) > 0 {
		u.defaultEfforts[key] = defs
	}
}

// doJSON performs a request and unwraps the gateway envelope.
func (u *Upstream) doJSON(req *http.Request) (json.RawMessage, error) {
	client := u.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 200))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{
			Kind:   kind,
			Status: resp.StatusCode,
			Msg:    fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160)),
		}
	}
	return env.Data, nil
}

// RefreshToken exchanges the refresh token for a new access token.  The network
// call happens outside the account lock; if another goroutine refreshed
// concurrently we adopt its result instead of overwriting it.
func (u *Upstream) RefreshToken(a *Auth) error {
	if a == nil {
		return errors.New("refresh: nil account")
	}
	a.mu.Lock()
	rtSnapshot := a.RefreshToken
	atBefore := a.AccessToken
	a.mu.Unlock()
	if strings.TrimSpace(rtSnapshot) == "" {
		return errors.New("refresh: no refreshToken")
	}

	endpoint := u.chatBase(a) + refreshTokenPath
	ctx, cancel := context.WithTimeout(context.Background(), refreshIOTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	hdr := &Auth{
		AccessToken:  atBefore,
		RefreshToken: rtSnapshot,
		ExpiresAt:    a.ExpiresAt,
		Domain:       a.Domain,
		UID:          a.UID,
		EnterpriseID: a.EnterpriseID,
		Nickname:     a.Nickname,
		DeviceToken:  a.DeviceToken,
	}
	u.RefreshHeaders(req, hdr)
	data, err := u.doJSON(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil {
		return fmt.Errorf("refresh: parse response: %w", err)
	}
	if strings.TrimSpace(tok.AccessToken) == "" {
		return errors.New("refresh_failed: no accessToken in response — re-login required")
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.AccessToken != atBefore && a.RefreshToken != rtSnapshot {
		// Someone else already refreshed; keep their fresher values.
		return nil
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	if tok.ExpiresIn > 0 {
		if d := time.Duration(tok.ExpiresIn) * time.Second; d < refreshTokenExpiresInMax {
			a.ExpiresAt = time.Now().Add(d).Unix()
		}
	}
	return nil
}

// ChatStream posts a chat body.  On success it returns the live response body
// (already wrapped with the idle watchdog) and a nil error.  On an upstream
// failure it returns the classified error plus the raw body for diagnostics.
func (u *Upstream) ChatStream(ctx context.Context, a *Auth, body []byte, clientIP string, meta ChatMeta) (io.ReadCloser, int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	prepared := u.prepareBody(body, a.RealmName(), a.UIDValue(), meta.ConversationID)
	if u.globalOn(a) {
		prepared = ensureConsoleSystem(prepared)
	}
	var lastStatus int
	var lastBody []byte
	var lastErr error
	for _, path := range u.chatPaths(a) {
		endpoint := u.chatBase(a) + path
		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(prepared))
		if err != nil {
			return nil, 0, nil, err
		}
		u.ChatHeaders(req, a, clientIP, meta)
		reqCtx, cancel := context.WithCancel(ctx)
		req = req.WithContext(reqCtx)
		client := u.chatHTTP()
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			roundTripCloseIdle(client.Transport)
			return nil, 0, nil, err
		}
		if resp.StatusCode >= 400 {
			raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
			resp.Body.Close()
			cancel()
			if rerr != nil {
				return nil, resp.StatusCode, nil, rerr
			}
			kind := Classify(resp.StatusCode, string(raw))
			if kind == ErrNone {
				return nil, resp.StatusCode, raw, nil
			}
			ue := &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
			if d, ok := ParseRetryAfter(resp.Header); ok {
				ue.RetryAfter = d
			}
			if kind == ErrSoftRate {
				// A soft rate limit is the one answer whose body dates itself,
				// so parse the instant the vendor promises instead of guessing.
				if resetAt, ok := ParseRateReset(string(raw)); ok {
					ue.ResetAt = resetAt
				}
				// Code 6004 is the model-scoped flavour: this account may serve
				// every other model, so the pool must park the model rather
				// than the credential.  Kind stays ErrSoftRate -- existing
				// callers and hints keep seeing exactly what they saw before.
				if IsModelRateLimit(string(raw)) {
					ue.ModelScoped = true
				}
			}
			return nil, resp.StatusCode, raw, ue
		}
		return monitorBody(resp.Body, u.IdleTimeout, cancel), resp.StatusCode, nil, nil
	}
	return nil, lastStatus, lastBody, lastErr
}

// --- models ----------------------------------------------------------------

// ModelInfo is one model as reported by the upstream catalogue.
type ModelInfo struct {
	ID   string
	Name string

	ContextWindow int64
	MaxTokens     int64

	Efforts       []string
	DefaultEffort string

	Description string
	Credits     string
	Tags        []string
	Vendor      string

	IsDefault          bool
	SupportsReasoning  bool
	SupportsToolCall   bool
	OnlyReasoning      bool
	SupportsImages     bool
	CanDisableThinking bool

	MaxAllowedSize   int64
	ReasoningEffort  string
	ReasoningSummary string

	PromoFactor  *float64
	PromoCredits string
	PromoLabel   string
	PromoNote    string
}

type dynModelReasoning struct {
	Effort             string   `json:"effort"`
	Summary            string   `json:"summary"`
	DefaultEffort      string   `json:"defaultEffort"`
	CanDisableThinking bool     `json:"canDisableThinking"`
	SupportedEfforts   []string `json:"supportedEfforts"`
}

type dynModelEntry struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	ModelID     string   `json:"modelId"`
	Model       string   `json:"model"`
	Description string   `json:"descriptionZh"`
	Credits     string   `json:"credits"`
	Tags        []string `json:"tags"`
	Vendor      string   `json:"vendor"`

	IsDefault bool `json:"isDefault"`

	MaxInputTokens  int64 `json:"maxInputTokens"`
	MaxOutputTokens int64 `json:"maxOutputTokens"`
	MaxAllowedSize  int64 `json:"maxAllowedSize"`

	Disabled bool `json:"disabled"`

	SupportsImages bool              `json:"supportsImages"`
	SupportsReason bool              `json:"supportsReasoning"`
	SupportsTool   bool              `json:"supportsToolCall"`
	OnlyReasoning  bool              `json:"onlyReasoning"`
	Reasoning      dynModelReasoning `json:"reasoning"`
}

func (e dynModelEntry) modelInfo() ModelInfo {
	def := e.Reasoning.DefaultEffort
	if def == "" {
		def = e.Reasoning.Effort
	}
	id := e.ID
	if id == "" {
		id = e.ModelID
	}
	if id == "" {
		id = e.Model
	}
	return ModelInfo{
		ID:                 id,
		Name:               e.Name,
		ContextWindow:      e.MaxInputTokens,
		MaxTokens:          e.MaxOutputTokens,
		Efforts:            e.Reasoning.SupportedEfforts,
		DefaultEffort:      def,
		Description:        e.Description,
		Credits:            e.Credits,
		Tags:               e.Tags,
		Vendor:             e.Vendor,
		IsDefault:          e.IsDefault,
		SupportsReasoning:  e.SupportsReason,
		SupportsToolCall:   e.SupportsTool,
		OnlyReasoning:      e.OnlyReasoning,
		SupportsImages:     e.SupportsImages,
		MaxAllowedSize:     e.MaxAllowedSize,
		CanDisableThinking: e.Reasoning.CanDisableThinking,
		ReasoningEffort:    e.Reasoning.Effort,
		ReasoningSummary:   e.Reasoning.Summary,
	}
}

// nonChatModel filters out models that are not chat completions endpoints.
func nonChatModel(id string, maxOutputTokens int64, tags []string) bool {
	lid := strings.ToLower(strings.TrimSpace(id))
	for _, p := range []string{"nes-", "completion-", "codewise-"} {
		if strings.HasPrefix(lid, p) {
			return true
		}
	}
	if maxOutputTokens > 0 && maxOutputTokens <= 256 {
		return true
	}
	for _, t := range tags {
		if strings.EqualFold(strings.TrimSpace(t), "text-to-image") {
			return true
		}
	}
	return false
}

// mergeModelInfos merges two catalogues, primary first, deduped by id.
func mergeModelInfos(primary, secondary []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(primary)+len(secondary))
	seen := map[string]bool{}
	for _, mi := range primary {
		if mi.ID == "" || seen[mi.ID] {
			continue
		}
		seen[mi.ID] = true
		out = append(out, mi)
	}
	for _, mi := range secondary {
		if mi.ID == "" || seen[mi.ID] {
			continue
		}
		seen[mi.ID] = true
		out = append(out, mi)
	}
	return out
}

// FetchModels discovers the catalogue from both upstream sources.  It succeeds
// when at least one of them answers.
func (u *Upstream) FetchModels(a *Auth) ([]ModelInfo, error) {
	type result struct {
		infos []ModelInfo
		err   error
	}
	// Each probe publishes its outcome by sending on its own buffered channel.
	// A WaitGroup would not do here: core.GoSafe runs the report only after fn's
	// own defers, so a deferred wg.Done would release the caller before the
	// report wrote ent/v3 -- a data race the race detector would only see on the
	// panic path.  A channel send is the last thing on both paths, so the
	// receive below is a real happens-before edge.
	entCh := make(chan result, 1)
	v3Ch := make(chan result, 1)
	core.GoSafe("workbuddy enterprise catalogue probe", func(msg string) {
		// Recording the panic as this source's error keeps FetchModels' "one
		// source answering is enough" contract instead of turning a panic into
		// a caller that waits for a result that never comes.
		entCh <- result{err: errors.New(msg)}
	}, func() {
		// A global account's enterprise catalogue is served from that realm's
		// own cache (1h TTL, 5 min failure cooldown).  Probing the vendor
		// directly on every refresh is exactly what the cache exists to
		// prevent, and keeping it realm-scoped stops one catalogue from
		// answering for the other.  A CN account -- or a global one whose cache
		// is cold and whose probe just failed -- falls through to the live
		// probe, so this can only ever add a cache hit, never remove an answer.
		if _, infos := u.fetchGlobalModelsOnce(a); len(infos) > 0 {
			entCh <- result{infos: infos}
			return
		}
		infos, err := u.fetchEnterpriseModels(a)
		entCh <- result{infos: infos, err: err}
	})
	core.GoSafe("workbuddy /v3/config catalogue probe", func(msg string) {
		v3Ch <- result{err: errors.New(msg)}
	}, func() {
		infos, err := u.fetchV3Models(a)
		v3Ch <- result{infos: infos, err: err}
	})
	ent, v3 := <-entCh, <-v3Ch

	if ent.err != nil && v3.err != nil {
		return nil, ent.err
	}
	if v3.err != nil {
		u.log("workbuddy: /v3/config model probe failed: %v", v3.err)
	}
	if ent.err != nil {
		u.log("workbuddy: enterprise model probe failed: %v", ent.err)
	}
	out := mergeModelInfos(v3.infos, ent.infos)
	if len(out) == 0 {
		return nil, errors.New("models api returned empty list")
	}
	cache := map[string][]string{}
	defCache := map[string]string{}
	for _, mi := range out {
		if len(mi.Efforts) > 0 {
			cache[mi.ID] = mi.Efforts
		}
		if mi.DefaultEffort != "" {
			defCache[mi.ID] = mi.DefaultEffort
		}
	}
	if len(cache) > 0 || len(defCache) > 0 {
		u.storeEfforts(a.RealmName(), cache, defCache)
	}
	return out, nil
}

// fetchEnterpriseModels reads the per-enterprise model list.  The probe has more
// than one path and the paths do not serve the same catalogue (the /v2 spelling
// is the only one that serves gpt-5.3-codex), so they are tried in order and the
// first usable answer wins.
func (u *Upstream) fetchEnterpriseModels(a *Auth) ([]ModelInfo, error) {
	var lastErr error
	ctx, cancel := context.WithTimeout(context.Background(), globalModelsProbeTimeout)
	defer cancel()
	for _, path := range globalModelsProbePaths {
		out, err := u.fetchEnterpriseModelsAt(ctx, a, path)
		if err == nil && len(out) > 0 {
			return out, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = errors.New("no usable cli models found")
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no enterprise model path answered")
	}
	return nil, lastErr
}

// fetchEnterpriseModelsAt reads one enterprise model path.
func (u *Upstream) fetchEnterpriseModelsAt(ctx context.Context, a *Auth, path string) ([]ModelInfo, error) {
	endpoint := u.chatBase(a) + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	u.CommonHeaders(req, a)
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	}
	client := u.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, modelsCacheResponseLimit))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Models []dynModelEntry `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("models: parse: %w", err)
	}
	if payload.Code != 0 {
		return nil, fmt.Errorf("models: upstream code %d", payload.Code)
	}
	var cliIDs []string
	for _, ag := range payload.Data.Agents {
		if strings.EqualFold(strings.TrimSpace(ag.Name), "cli") {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, errors.New("no cli agent models found")
	}
	byID := map[string]ModelInfo{}
	for _, e := range payload.Data.Models {
		mi := e.modelInfo()
		if mi.ID == "" || nonChatModel(mi.ID, e.MaxOutputTokens, e.Tags) {
			continue
		}
		byID[mi.ID] = mi
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		mi, ok := byID[id]
		if !ok || mi.ID == "" {
			continue
		}
		out = append(out, mi)
	}
	if len(out) == 0 {
		return nil, errors.New("no usable cli models found")
	}
	return out, nil
}

// fetchV3Models reads the /v3/config catalogue for both official user agents
// concurrently and unions the answers.  Neither UA is served a superset of the
// other, so a single-UA read silently loses models without ever erroring.
func (u *Upstream) fetchV3Models(a *Auth) ([]ModelInfo, error) {
	type result struct {
		out []ModelInfo
		err error
	}
	// Bounded so the wg.Wait below is on requests that can actually be
	// cancelled; the shared deadline covers both legs and the merge.
	ctx, cancel := context.WithTimeout(context.Background(), globalModelsProbeTimeout)
	defer cancel()
	probe := func(ua string) result {
		byID, err := u.fetchV3ConfigModelMap(ctx, a, ua)
		if err != nil {
			return result{err: err}
		}
		out := make([]ModelInfo, 0, len(byID))
		for id, mi := range byID {
			if nonChatModel(id, mi.MaxTokens, mi.Tags) {
				continue
			}
			out = append(out, mi)
		}
		if len(out) == 0 {
			return result{err: errors.New("v3 config returned no chat models")}
		}
		return result{out: out}
	}

	// Same channel-instead-of-WaitGroup shape as FetchModels: core.GoSafe runs
	// the report after fn's defers, so a deferred wg.Done would publish ide/cli
	// to the caller before the report wrote them.
	ideCh := make(chan result, 1)
	cliCh := make(chan result, 1)
	core.GoSafe("workbuddy v3/config IDE-UA probe", func(msg string) { ideCh <- result{err: errors.New(msg)} },
		func() { ideCh <- probe(codeBuddyIDEUA) })
	core.GoSafe("workbuddy v3/config CLI-UA probe", func(msg string) { cliCh <- result{err: errors.New(msg)} },
		func() { cliCh <- probe(codeBuddyCLIUA) })
	ide, cli := <-ideCh, <-cliCh

	switch {
	case ide.err != nil && cli.err != nil:
		return nil, ide.err
	case ide.err != nil:
		u.log("workbuddy: v3/config IDE-UA probe failed (CLI-UA only): %v", ide.err)
		return cli.out, nil
	case cli.err != nil:
		u.log("workbuddy: v3/config CLI-UA probe failed (IDE-UA only): %v", cli.err)
		return ide.out, nil
	}
	return mergeModelInfos(ide.out, cli.out), nil
}

type v3ModelPromotion struct {
	Enabled  bool     `json:"enabled"`
	Priority int      `json:"priority"`
	ModelIDs []string `json:"modelIds"`
	Badge    *struct {
		Label string `json:"label"`
	} `json:"badge"`
	Discount *struct {
		DiscountedCredits string  `json:"discountedCredits"`
		Factor            float64 `json:"factor"`
	} `json:"discount"`
	Hover *struct {
		TextZh string `json:"textZh"`
	} `json:"hover"`
	Schedule *struct {
		Daily []struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"daily"`
		Timezone   string `json:"timezone"`
		ValidFrom  string `json:"validFrom"`
		ValidUntil string `json:"validUntil"`
	} `json:"schedule"`
}

// fetchV3ConfigModelMap reads /v3/config with the official IDE UA and returns
// the catalogue keyed by model id.
func (u *Upstream) fetchV3ConfigModelMap(ctx context.Context, a *Auth, ua string) (map[string]ModelInfo, error) {
	endpoint := u.chatBase(a) + v3ConfigPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	u.CommonHeaders(req, a)
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	}
	client := u.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, modelsCacheResponseLimit))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Models json.RawMessage `json:"models"`
			// The campaigns arrive as an array, but the /v3/config envelope is
			// polymorphic elsewhere, so the shape is resolved rather than
			// guessed: a wrong guess here would fail the whole parse and take
			// the catalogue down with it.
			ModelPromotions json.RawMessage `json:"modelPromotions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("v3 config: parse: %w", err)
	}
	if payload.Code != 0 {
		return nil, fmt.Errorf("v3 config: upstream code %d", payload.Code)
	}
	byID := map[string]ModelInfo{}
	if len(payload.Data.Models) > 0 {
		var asMap map[string]dynModelEntry
		if json.Unmarshal(payload.Data.Models, &asMap) == nil {
			for _, e := range asMap {
				mi := e.modelInfo()
				if mi.ID != "" {
					byID[mi.ID] = mi
				}
			}
		} else {
			var asList []dynModelEntry
			if json.Unmarshal(payload.Data.Models, &asList) == nil {
				for _, e := range asList {
					mi := e.modelInfo()
					if mi.ID != "" {
						byID[mi.ID] = mi
					}
				}
			}
		}
	}
	applyModelPromotions(byID, parseModelPromotions(payload.Data.ModelPromotions))
	return byID, nil
}

// parseModelPromotions resolves the campaign list from either envelope the
// upstream has been seen to use: a bare array (the shape the reference reads) or
// an object keyed by campaign id.  A map is flattened in sorted-key order so the
// tie-break below stays deterministic.  An unrecognised shape yields nothing
// rather than an error: campaigns are decoration, and losing them must never
// cost the catalogue.
func parseModelPromotions(raw json.RawMessage) []v3ModelPromotion {
	if len(raw) == 0 {
		return nil
	}
	var list []v3ModelPromotion
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var asMap map[string]v3ModelPromotion
	if json.Unmarshal(raw, &asMap) != nil || len(asMap) == 0 {
		return nil
	}
	keys := make([]string, 0, len(asMap))
	for k := range asMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]v3ModelPromotion, 0, len(keys))
	for _, k := range keys {
		out = append(out, asMap[k])
	}
	return out
}

// applyModelPromotions attaches promotional metadata to the affected models.
//
// A model can be covered by several campaigns at once — the live catalogue runs
// a badge-only one during the day and a half-price one at night, told apart by
// priority and the daily window — so the winner is the active promotion with the
// highest priority.  On an equal priority the earlier campaign in catalogue
// order wins, which is what the reference does and keeps the outcome stable.
//
// A promotion with no discount object (an off-peak campaign) still contributes
// its badge and hover text and leaves PromoFactor nil, which the panel reads as
// "no price change, just a label".
func applyModelPromotions(byID map[string]ModelInfo, promos []v3ModelPromotion) {
	applyModelPromotionsAt(byID, promos, time.Now().In(promoZone))
}

// applyModelPromotionsAt is applyModelPromotions with the clock passed in, so the
// schedule logic can be tested without waiting for the right hour.
func applyModelPromotionsAt(byID map[string]ModelInfo, promos []v3ModelPromotion, now time.Time) {
	if len(byID) == 0 || len(promos) == 0 {
		return
	}
	type cand struct {
		prio int
		p    *v3ModelPromotion
	}
	best := map[string]cand{}
	for i := range promos {
		p := &promos[i]
		if !promoActive(p, now) {
			continue
		}
		for _, id := range p.ModelIDs {
			if _, ok := byID[id]; !ok {
				continue // outside this catalogue (e.g. a same-named global variant)
			}
			if b, seen := best[id]; !seen || p.Priority > b.prio {
				best[id] = cand{prio: p.Priority, p: p}
			}
		}
	}
	for id, c := range best {
		mi := byID[id]
		if c.p.Badge != nil {
			mi.PromoLabel = c.p.Badge.Label
		}
		if c.p.Hover != nil {
			mi.PromoNote = c.p.Hover.TextZh
		}
		if c.p.Discount != nil {
			f := c.p.Discount.Factor
			mi.PromoFactor = &f
			mi.PromoCredits = c.p.Discount.DiscountedCredits
		}
		byID[id] = mi
	}
}

// truncate shortens a diagnostic string.
func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
