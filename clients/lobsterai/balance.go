package lobsterai

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"client2api/internal/core"
)

// AccountBalance reads the account's remaining credits.
//
// The endpoint is /api/user/profile-summary, NOT /api/user/quota.
//
// quota reports only freeCreditsTotal -- about 300 -- and omits the activity
// credits, which is where the bulk of a real balance (measured around 5000)
// lives.  An account that has thousands of credits to spend would be shown as
// nearly empty, which is worse than showing nothing.  profile-summary's
// data.totalCreditsRemaining is the number the vendor's own client displays.
//
// A negative balance is clamped to zero: the vendor has been seen returning a
// small negative number, and "-3 credits" is not a thing the panel can render
// meaningfully.
//
// Unlike Checkin, a failure here IS returned as a Go error: the panel has
// nothing sensible to display without a number, so it should answer with an
// error rather than a fabricated zero.
func (c *Client) AccountBalance(ctx context.Context, id string, soon time.Duration) (core.Balance, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ensure()
	acct := c.pool.byID(id)
	if acct == nil {
		return core.Balance{}, fmt.Errorf("account %q not found", id)
	}
	if acct.AccessToken == "" {
		return core.Balance{}, fmt.Errorf("account %q has no access token; sign in again", id)
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.modelsTimeout())
	defer cancel()
	if err := c.ensureFresh(ctx, acct); err != nil {
		return core.Balance{}, err
	}
	version := c.clientVersion(ctx)
	spec := requestSpec{
		method:  http.MethodGet,
		url:     c.cfg.profileSummaryURL(),
		bearer:  acct.AccessToken,
		version: version,
		timeout: c.cfg.modelsTimeout(),
	}
	status, body, err := c.doJSON(ctx, spec)
	if err != nil {
		return core.Balance{}, c.classifyErrFor(acct, err)
	}
	if status != http.StatusOK {
		return core.Balance{}, c.classifyHTTP("balance", acct.ID, status, body)
	}
	raw, err := decodeEnvelope("balance", body)
	if err != nil {
		return core.Balance{}, err
	}
	obj := objectOf(anyOf(raw))
	if obj == nil {
		return core.Balance{}, fmt.Errorf("balance: the response data was not an object")
	}
	credits, ok := firstIntOK(obj, "totalCreditsRemaining")
	if !ok {
		return core.Balance{}, fmt.Errorf("balance: the response carried no totalCreditsRemaining")
	}
	if credits < 0 {
		credits = 0
	}
	bal := core.Balance{
		Credits: int64(credits),
		Total:   int64(credits),
		Unit:    "积分",
	}
	c.noteSuccess(acct)

	// creditItems[] is a list of {type, creditsRemaining, expiresAt}.  It is
	// what makes the "expiring soon" bucket possible; the total above already
	// includes all of them.
	now := c.now()
	var earliest time.Time
	for _, item := range listOf(obj["creditItems"]) {
		m := objectOf(item)
		if m == nil {
			continue
		}
		remaining := int64(firstInt(m, "creditsRemaining", "credits"))
		exp, ok := parseExpiryLoose(firstString(m, "expiresAt", "expireAt", "expiredAt"))
		if !ok {
			continue
		}
		if earliest.IsZero() || exp.Before(earliest) {
			earliest = exp
			bal.EarliestRemaining = remaining
		}
		if soon > 0 && exp.Before(now.Add(soon)) {
			bal.Expiring += remaining
		}
	}
	if !earliest.IsZero() {
		bal.EarliestAt = earliest
	}
	return bal, nil
}

// parseExpiryLoose is parseExpiry plus the space-separated spellings the
// account endpoints sometimes use for expiry timestamps.
func parseExpiryLoose(raw string) (time.Time, bool) {
	if t, ok := parseExpiry(raw); ok {
		return t, true
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	} {
		if t, err := time.ParseInLocation(layout, raw, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

var _ core.BalanceProvider = (*Client)(nil)
