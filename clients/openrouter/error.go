package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"client2api/internal/core"
)

// failureKind is this module's own, finer vocabulary.  The vendor's codes are
// richer than core.FailureKind, and collapsing them early would lose the
// distinction between "this key is out of money" (park for a while) and "this
// model does not exist" (do not blame the account at all).
type failureKind string

const (
	kindNone       failureKind = ""
	kindCredit     failureKind = "credit"     // out of credit / payment required
	kindDailyQuota failureKind = "daily"      // free-tier daily request cap
	kindRateLimit  failureKind = "rate_limit" // transient throttling
	kindAuth       failureKind = "auth"       // the key itself is rejected
	kindNotFound   failureKind = "not_found"  // no endpoint serves this model
	kindBlocked    failureKind = "blocked"    // moderation / content policy
	kindServer     failureKind = "server"     // 5xx
	kindClient     failureKind = "client"     // any other 4xx
)

// Marker tables.  Both English and Chinese spellings are listed because the
// vendor and its upstream providers emit both.
var (
	creditMarkers = []string{
		"insufficient credit", "insufficient balance", "not enough credit",
		"out of credit", "no credit", "requires more credit", "add credit",
		"credit limit", "payment required", "exceeded your current quota",
		"quota exceeded", "insufficient_quota", "billing",
		"余额不足", "额度不足", "欠费", "积分不足",
	}
	dailyQuotaMarkers = []string{
		"free-models-per-day", "free models per day", "free model requests",
		"daily limit", "per-day limit", "resets at midnight",
	}
	rateMarkers = []string{
		"rate limit", "rate-limit", "ratelimit", "too many requests",
		"requests per", "temporarily rate-limited", "slow down", "try again in",
		"请求过于频繁", "频率限制", "限流",
	}
	authMarkers = []string{
		"no cookie auth credentials", "no auth credentials", "missing authentication",
		"invalid api key", "invalid_api_key", "api key not found", "invalid key",
		"key was disabled", "key has been disabled", "revoked", "unauthorized",
		"authentication", "invalid credentials", "user not found",
		"未授权", "密钥无效", "鉴权失败",
	}
	notFoundMarkers = []string{
		"no endpoints found", "no allowed providers", "model not found",
		"not a valid model", "unknown model", "invalid model", "does not exist",
		"no such model", "model_unavailable",
	}
	blockedMarkers = []string{
		"flagged", "moderation", "moderated", "content policy", "content_policy",
		"prohibited", "violates", "safety", "内容审核", "违规",
	}
)

// containsMarker searches the lower-cased text (so English markers match
// regardless of case) and the original (so the Chinese ones, which have no
// case, match too).
func containsMarker(text string, markers []string) bool {
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	for _, m := range markers {
		if m == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// classifyFailure is a pure function of the status, the vendor's numeric code
// and the message text, with a fixed precedence: an explicit status first, then
// text markers, then the status class.  Text wins over the status class because
// OpenRouter reports several quite different problems through 403.
func classifyFailure(status, code int, text string) failureKind {
	if status == 402 {
		return kindCredit
	}
	if containsMarker(text, blockedMarkers) {
		return kindBlocked
	}
	if containsMarker(text, dailyQuotaMarkers) {
		return kindDailyQuota
	}
	if containsMarker(text, creditMarkers) {
		return kindCredit
	}
	if containsMarker(text, notFoundMarkers) {
		return kindNotFound
	}
	if containsMarker(text, authMarkers) {
		return kindAuth
	}
	if containsMarker(text, rateMarkers) {
		return kindRateLimit
	}
	switch {
	case status == 401 || status == 403:
		return kindAuth
	case status == 429:
		return kindRateLimit
	case status == 404:
		return kindNotFound
	case status >= 500:
		return kindServer
	case status >= 400:
		return kindClient
	}
	_ = code
	return kindNone
}

// coreKindFor maps the fine verdict onto the gateway's vocabulary.  `notFound`
// and `client` land on Other on purpose: this module has already rotated
// internally, so calling them retryable would invite the gateway to hammer.
func coreKindFor(k failureKind) core.FailureKind {
	switch k {
	case kindCredit, kindDailyQuota:
		return core.FailureQuota
	case kindRateLimit:
		return core.FailureRateLimited
	case kindAuth:
		return core.FailureAuth
	case kindServer:
		return core.FailureUpstream
	case kindBlocked:
		return core.FailureContentBlocked
	default:
		return core.FailureOther
	}
}

// --- error envelope -------------------------------------------------------

// orErrorEnvelope is the vendor's error shape: `{"error":{"message":…,"code":…}}`.
// It is NOT the OpenAI envelope, so parsing it as one would silently lose the
// message.
type orErrorEnvelope struct {
	Error struct {
		Message  string         `json:"message"`
		Code     any            `json:"code"`
		Metadata map[string]any `json:"metadata"`
	} `json:"error"`
	Message string `json:"message"`
}

// parseErrorEnvelope extracts the vendor's code and message.  Either may be
// absent: a proxy or a truncated body can produce a non-envelope.
func parseErrorEnvelope(body []byte) (int, string) {
	var env orErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return 0, ""
	}
	code, _ := toInt(env.Error.Code)
	msg := strings.TrimSpace(env.Error.Message)
	if msg == "" {
		msg = strings.TrimSpace(env.Message)
	}
	if msg == "" {
		if et, ok := env.Error.Metadata["error_type"].(string); ok {
			msg = strings.TrimSpace(et)
		}
	}
	return code, msg
}

// errorTextOf is the message an operator will read.  It always falls back to a
// truncated raw body so a non-envelope failure is still diagnosable.
func errorTextOf(body []byte) string {
	_, msg := parseErrorEnvelope(body)
	if msg != "" {
		return truncate(msg, 300)
	}
	return truncate(strings.TrimSpace(string(body)), 300)
}

// upstreamError is the transport-level detail wrapped inside a core.Failure.
type upstreamError struct {
	Op     string
	Status int
	Msg    string
}

func (e *upstreamError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("%s: %s (HTTP %d): %s", clientName, e.Op, e.Status, e.Msg)
	}
	return fmt.Sprintf("%s: %s: %s", clientName, e.Op, e.Msg)
}

// classifyHTTP turns a non-2xx response into a typed failure, and records the
// account-side consequence.
func (c *Client) classifyHTTP(op, accountID string, status int, body []byte) error {
	code, msg := parseErrorEnvelope(body)
	if msg == "" {
		msg = truncate(strings.TrimSpace(string(body)), 300)
	}
	k := classifyFailure(status, code, msg)
	msg = c.scrubFor(accountID, msg)
	c.noteFailure(accountID, k, msg)
	return core.Fail(clientName, accountID, coreKindFor(k), status, &upstreamError{Op: op, Status: status, Msg: msg})
}

// scrubFor strips anything credential-shaped out of a message before it can
// reach a log, the panel or a client.
//
// core.Redact only catches the LABELLED forms ("api_key=…", "Bearer …"), and a
// vendor that echoes the bare key back inside a sentence ("invalid key
// sk-or-v1-…") would slip through it.  The pool knows the literal key for an
// account, so the exact string is masked as well.
func (c *Client) scrubFor(accountID, msg string) string {
	msg = core.Redact(msg)
	if accountID != "" && c.pool != nil {
		if rec, ok := c.pool.byID(accountID); ok && rec.APIKey != "" {
			msg = strings.ReplaceAll(msg, rec.APIKey, core.MaskSecret(rec.APIKey))
		}
	}
	return truncate(msg, 300)
}

// classifyUpstream turns a transport error into a typed failure.  A failure
// that classifies as "nothing is wrong with the account" is deliberately
// re-labelled as a server-side problem: a credential whose network path is
// broken is worth resting, and core.Retryable(FailureUpstream) is true.
func (c *Client) classifyUpstream(accountID string, err error) error {
	if err == nil {
		return nil
	}
	k := kindNone
	if isTimeout(err) {
		k = kindServer
	} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		k = kindNone
	}
	msg := truncate(core.Redact(err.Error()), 300)
	if k == kindNone {
		k = kindServer
	}
	c.noteFailure(accountID, k, msg)
	return core.Fail(clientName, accountID, coreKindFor(k), 0, &upstreamError{Op: "request", Msg: msg})
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// --- consequences ---------------------------------------------------------

// noteFailure records what a verdict means for the account.  Only the verdicts
// that are genuinely about capacity write a cooldown: a panel badge that says
// "rate limited for an hour" after a 400 is worse than no badge at all.
func (c *Client) noteFailure(accountID string, k failureKind, msg string) {
	if accountID == "" {
		return
	}
	now := c.now()
	redacted := c.scrubFor(accountID, msg)
	switch k {
	case kindCredit:
		c.pool.setCooldown(accountID, now.Add(c.cfg.quotaCooldown()), redacted)
		c.pool.setNote(accountID, "out of credit: "+redacted)
	case kindDailyQuota:
		c.pool.setCooldown(accountID, now.Add(dailyQuotaPark(now)), redacted)
		c.pool.setNote(accountID, "free-tier daily cap reached: "+redacted)
	case kindRateLimit:
		c.pool.setCooldown(accountID, now.Add(c.cfg.rateCooldown()), redacted)
	case kindAuth:
		c.pool.setCooldown(accountID, now.Add(c.cfg.authCooldown()), redacted)
		c.pool.markInvalid(accountID, true)
		c.pool.setNote(accountID, "the vendor rejected this key: "+redacted)
	case kindNotFound, kindBlocked:
		// Neither says anything about the credential: a model that does not
		// exist and a moderated prompt must not park a healthy key.
		c.pool.setNote(accountID, redacted)
	default:
		c.pool.noteError(accountID, redacted, now, c.parkThreshold(), c.cooldown())
	}
	c.persistState()
}

// dailyQuotaPark parks a free-tier account until the vendor's daily counter
// resets, clamped so a clock skew can neither release it immediately nor park
// it for days.
func dailyQuotaPark(now time.Time) time.Duration {
	next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
	d := next.Sub(now)
	if d < time.Hour {
		return time.Hour
	}
	if d > 24*time.Hour {
		return 24 * time.Hour
	}
	return d
}

// noteSuccess clears the account's verdicts after a good response.
func (c *Client) noteSuccess(acct accountRecord) {
	now := c.now()
	c.pool.touchLastUsed(acct.ID, now)
	if c.pool.noteSuccess(acct.ID) {
		c.persistState()
	}
}

// failureKindOfError is the read-only view of an already-classified error.
func failureKindOfError(err error) failureKind {
	if err == nil {
		return kindNone
	}
	var f *core.Failure
	if !errors.As(err, &f) {
		return kindNone
	}
	switch f.Kind {
	case core.FailureQuota:
		return kindCredit
	case core.FailureRateLimited:
		return kindRateLimit
	case core.FailureAuth, core.FailureSessionDead:
		return kindAuth
	case core.FailureContentBlocked:
		return kindBlocked
	case core.FailureUpstream:
		return kindServer
	}
	return kindNone
}

// describeError is the single line Status()/Detail carries.
func (c *Client) describeError(err error) string {
	if err == nil {
		return ""
	}
	return truncate(core.Redact(err.Error()), 300)
}
