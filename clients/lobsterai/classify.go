package lobsterai

import (
	"encoding/json"
	"errors"
	"strings"

	"client2api/internal/core"
)

// failureKind is this module's classification of one upstream refusal.
//
// It exists as a separate type from core.FailureKind because the vendor's
// vocabulary is finer than the gateway's: "the balance is exhausted" and "the
// model id does not exist" are both terminal for the account in play, but they
// want different cooldowns and different panel text.
type failureKind string

const (
	kindNone        failureKind = "none"
	kindHardCredit  failureKind = "hard-credit"
	kindSoftRate    failureKind = "soft-rate"
	kindSessionDead failureKind = "session-dead"
	kindNotFound    failureKind = "not-found"
	kindServer      failureKind = "server"
	kindClient      failureKind = "client"
)

// hardCreditMarkers are the credit-exhaustion spellings the vendor and its
// proxies use.  Both languages are listed because the message follows the
// account's locale.
var hardCreditMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit", "freecreditsused", "free credits used",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分", "积分耗尽",
}

// sessionDeadMarkers mean "this credential will never work again": the account
// must be disabled rather than retried.
var sessionDeadMarkers = []string{
	"40100", "40101", "token rejected", "refresh token was rejected",
}

// sessionDeadCodes are the envelope codes that mean the same thing.
var sessionDeadCodes = map[int]bool{40100: true, 40101: true}

// classifyFailure is the pure classifier.  Precedence matters and is fixed:
// a 402 is a credit verdict no matter what the body says, and a credit verdict
// outranks a rate limit, because the two want different cooldowns.
//
// Both the raw text and its lower-cased form are searched: the English markers
// are case-insensitive, and the Chinese ones are not affected by case folding
// but are still found by the second channel.
func classifyFailure(status, code int, text string) failureKind {
	if status == 402 {
		return kindHardCredit
	}
	if sessionDeadCodes[code] {
		return kindSessionDead
	}
	if containsMarker(text, hardCreditMarkers) {
		return kindHardCredit
	}
	if containsMarker(text, sessionDeadMarkers) {
		return kindSessionDead
	}
	switch {
	case status == 401 || status == 403:
		return kindSessionDead
	case status == 429:
		return kindSoftRate
	case status == 404:
		return kindNotFound
	case status >= 500:
		return kindServer
	case status >= 400:
		return kindClient
	default:
		return kindNone
	}
}

// containsMarker searches a message for any marker, case-insensitively and in
// the original text.
func containsMarker(text string, markers []string) bool {
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	for _, m := range markers {
		if strings.Contains(lower, m) || strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// coreKindFor maps this module's vocabulary onto the gateway's.
//
// not-found and client both land on core.FailureOther: the shared package has
// no not-found kind, and neither verdict is something a caller should retry
// against the same account.  That is not a loss of information here, because
// the module has already rotated internally by the time a caller sees the
// error.
func coreKindFor(k failureKind) core.FailureKind {
	switch k {
	case kindHardCredit:
		return core.FailureQuota
	case kindSoftRate:
		return core.FailureRateLimited
	case kindSessionDead:
		return core.FailureSessionDead
	case kindServer:
		return core.FailureUpstream
	default:
		return core.FailureOther
	}
}

// errorTextOf extracts a human-readable reason from an error body, preferring
// the vendor's own message over the raw JSON.
func errorTextOf(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	var probe struct {
		Msg     string `json:"msg"`
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(trimmed), &probe); err == nil {
		if msg := firstNonEmpty(probe.Error.Message, probe.Message, probe.Msg); msg != "" {
			return msg
		}
	}
	return truncate(trimmed, 300)
}

// classifyHTTP turns a non-2xx response into a classified error.
func (c *Client) classifyHTTP(op, accountID string, status int, body []byte) error {
	text := errorTextOf(body)
	k := classifyFailure(status, 0, text)
	return core.Fail(clientName, accountID, coreKindFor(k), status, &upstreamError{
		Op:     op,
		Status: status,
		Msg:    text,
	})
}

// classifyUpstream turns a decoded envelope failure into a classified error.
func (c *Client) classifyUpstream(accountID string, err error) error {
	if err == nil {
		return nil
	}
	var ue *upstreamError
	if !errors.As(err, &ue) {
		return err
	}
	k := classifyFailure(ue.Status, ue.Code, ue.Msg)
	return core.Fail(clientName, accountID, coreKindFor(k), ue.Status, err)
}

// noteFailure teaches the pool what happened to an account.
//
// The cooldown policy is deliberately narrow: only the three verdicts that are
// genuinely about capacity write a cooldown, because a panel badge that says
// "rate limited for an hour" after a 400 is worse than no badge at all.
func (c *Client) noteFailure(acct *accountRecord, k failureKind, msg string) {
	if acct == nil {
		return
	}
	now := c.now()
	switch k {
	case kindHardCredit:
		c.pool.setCooldown(acct.ID, now.Add(c.cfg.quotaCooldown()), msg)
	case kindSoftRate:
		c.pool.setCooldown(acct.ID, now.Add(c.cfg.cooldown()), msg)
	case kindNotFound:
		// No error count: a model that does not exist is not the account's
		// fault, and three of them must not park a healthy credential.
		c.pool.setCooldown(acct.ID, now.Add(c.cfg.cooldown()), msg)
	case kindSessionDead:
		c.pool.disable(acct.ID, msg)
	default:
		c.pool.noteError(acct.ID, msg, now, softErrorThreshold, c.cfg.cooldown())
	}
	c.persist()
}

// softErrorThreshold is how many consecutive unclassified failures park an
// account.  Three is the vendor client's number.
const softErrorThreshold = 3

// noteSuccess clears an account's failure state after a served request.
func (c *Client) noteSuccess(acct *accountRecord) {
	if acct == nil {
		return
	}
	if c.pool.clearCooldown(acct.ID) {
		c.persist()
	}
}

// classifyErrFor notes the failure against an account and returns the
// classified error, so the caller can rotate and keep the LAST verdict.
func (c *Client) classifyErrFor(acct *accountRecord, err error) error {
	if err == nil {
		return nil
	}
	accountID := ""
	if acct != nil {
		accountID = acct.ID
	}
	var ue *upstreamError
	status, code, text := 0, 0, err.Error()
	if errors.As(err, &ue) {
		status, code, text = ue.Status, ue.Code, firstNonEmpty(ue.Msg, err.Error())
	}
	k := classifyFailure(status, code, text)
	if k == kindNone {
		// A transport error (a reset connection, a DNS failure) is not the
		// vendor refusing anything.  It still parks the account after a few in
		// a row, because a credential whose network path is broken is worth
		// resting, but it is classified as a server-side problem.
		k = kindServer
	}
	c.noteFailure(acct, k, text)
	return core.Fail(clientName, accountID, coreKindFor(k), status, err)
}

// failureKindOfError is the read-only counterpart, used where the caller only
// needs the verdict.
func failureKindOfError(err error) failureKind {
	if err == nil {
		return kindNone
	}
	var ue *upstreamError
	if errors.As(err, &ue) {
		return classifyFailure(ue.Status, ue.Code, firstNonEmpty(ue.Msg, err.Error()))
	}
	return kindServer
}

// describeError renders an error for a status note without leaking a token.
func (c *Client) describeError(err error) string {
	if err == nil {
		return ""
	}
	return truncate(core.Redact(err.Error()), 300)
}
