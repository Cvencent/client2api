package qwenwork

// balance.go exposes QwenWork's live credit balance to the panel.
//
// The desktop client reads one Account Service endpoint and then displays the
// user quota:
//
//	GET /api/v1/adapter/user/account-context?include=user,plan,quota,page,data_sharing
//
// The response is normally wrapped in `data` and nests the useful meter under
// `quota.user_quota`:
//
//	{"data":{"user":{"id":"u1"},"quota":{"user_quota":{"total":2000,
//	 "used":766,"remaining":1234,"unit":"credits"}}}}
//
// The vendor's own parser also accepts camelCase aliases and, when the response
// has no `user_quota` object, treats the `quota` object itself as the meter. We
// follow those rules rather than inventing a second balance model.

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"client2api/internal/core"
)

const (
	accountContextPath = "/api/v1/adapter/user/account-context?include=user,plan,quota,page,data_sharing"
	balanceTimeout     = 20 * time.Second
)

// accountContextEnvelope accepts both the wrapped and bare forms. Data is nil
// when the response has no object-valued data member.
type accountContextEnvelope struct {
	Data  *accountContextPayload `json:"data"`
	User  *accountContextUser    `json:"user"`
	Quota *accountContextQuota   `json:"quota"`
}

type accountContextPayload struct {
	User  *accountContextUser  `json:"user"`
	Quota *accountContextQuota `json:"quota"`
}

type accountContextUser struct {
	ID string `json:"id"`
}

// accountContextQuota includes the quota object's own fields and the two names
// the vendor client accepts for the primary user meter.
type accountContextQuota struct {
	UserQuota      *accountContextQuota `json:"user_quota"`
	UserQuotaCamel *accountContextQuota `json:"userQuota"`

	Total     *float64 `json:"total"`
	Used      *float64 `json:"used"`
	Remaining *float64 `json:"remaining"`
	Unit      string   `json:"unit"`
}

// AccountBalance implements core.BalanceProvider.
func (c *Client) AccountBalance(ctx context.Context, id string, _ time.Duration) (core.Balance, error) {
	if c == nil {
		return core.Balance{}, fmt.Errorf("qwenwork: no client")
	}

	e, err := c.checkinAccount(id)
	if err != nil {
		return core.Balance{}, fmt.Errorf("qwenwork: account balance: %w", err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, balanceTimeout)
	defer cancel()

	if e.acct.needsRefresh(time.Now(), c.cfg.refreshMargin()) {
		_ = c.tryRefresh(ctx, e)
	}

	var raw accountContextEnvelope
	if err := c.balanceDo(ctx, e, &raw); err != nil {
		return core.Balance{}, fmt.Errorf("qwenwork: account balance: %s", cleanErrorText(err.Error()))
	}
	c.noteUpstream(true, kindNone, "")

	bal, err := parseAccountContextBalance(raw)
	if err != nil {
		return core.Balance{}, err
	}
	return bal, nil
}

// balanceDo performs the account-context read and retries once after refreshing
// a rejected access token.
func (c *Client) balanceDo(ctx context.Context, e *entry, out any) error {
	rawURL := c.cfg.endpoint(accountContextPath)
	err := c.doJSON(ctx, "GET", rawURL, "", c.balanceHeaders(c.pool.accountOf(e)), out)
	if err == nil {
		return nil
	}
	ue, ok := asUpstreamError(err)
	if !ok || ue.Kind != kindAuth || c.pool.refreshTokenOf(e) == "" {
		return err
	}
	if refreshErr := c.tryRefresh(ctx, e); refreshErr != nil {
		return refreshErr
	}
	return c.doJSON(ctx, "GET", rawURL, "", c.balanceHeaders(c.pool.accountOf(e)), out)
}

func (c *Client) balanceHeaders(acct account) map[string]string {
	return map[string]string{
		"Accept":        "application/json",
		"Content-Type":  "application/json",
		"User-Agent":    c.cfg.userAgent(),
		"Authorization": "Bearer " + acct.AccessToken,
		"X-Request-Id":  newUUID(),
	}
}

func parseAccountContextBalance(raw accountContextEnvelope) (core.Balance, error) {
	user, quota := raw.User, raw.Quota
	if raw.Data != nil {
		user, quota = raw.Data.User, raw.Data.Quota
	}
	if user == nil || strings.TrimSpace(user.ID) == "" {
		return core.Balance{}, fmt.Errorf("qwenwork: account-context reply has no user id")
	}
	if quota == nil {
		return core.Balance{}, fmt.Errorf("qwenwork: account-context reply has no quota")
	}

	meter := quota.UserQuota
	if meter == nil {
		meter = quota.UserQuotaCamel
	}
	if meter == nil {
		meter = quota
	}
	if meter.Total == nil && meter.Used == nil && meter.Remaining == nil {
		return core.Balance{}, fmt.Errorf("qwenwork: account-context quota has no readable fields")
	}

	total := 0.0
	if meter.Total != nil {
		total = *meter.Total
	}
	used := 0.0
	if meter.Used != nil {
		used = *meter.Used
	}
	remaining := math.Max(total-used, 0)
	if meter.Remaining != nil {
		remaining = math.Max(*meter.Remaining, 0)
	}
	if !finite(total) || !finite(used) || !finite(remaining) {
		return core.Balance{}, fmt.Errorf("qwenwork: account-context quota contains a non-finite number")
	}

	return core.Balance{
		Credits: int64(math.Round(remaining)),
		Used:    used,
		Total:   int64(math.Round(math.Max(total, 0))),
		Unit:    qwenworkBalanceUnit(meter.Unit),
	}, nil
}

func qwenworkBalanceUnit(raw string) string {
	unit := strings.TrimSpace(raw)
	if unit == "" || strings.EqualFold(unit, "credits") || strings.EqualFold(unit, "credit") {
		return "积分"
	}
	return unit
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

var _ core.BalanceProvider = (*Client)(nil)
