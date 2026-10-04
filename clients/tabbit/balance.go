package tabbit

// balance.go exposes the vendor's own quota percentage to the panel.
//
// Tabbit's quota endpoint does not report a credit count.  It reports how much
// of the current quota has been consumed:
//
//	GET /api/commerce/quota/v1/usage?user_id=<uid>
//	{"member_level":"pro","usage_percentage":12.5,"remaining_reset_hours":5}
//
// The panel's account column renders core.Balance as "remaining / total" plus a
// progress bar.  Mapping the percentage onto that shape is honest only if the
// unit stays explicit, so this module reports:
//
//	Credits = 100 - usage_percentage   (remaining percent)
//	Total   = 100
//	Used    = usage_percentage
//	Unit    = "%"
//
// Only web-token accounts can answer: a sidecar endpoint is a local bridge and
// has no vendor account whose quota could be read.

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"client2api/internal/core"
)

// balanceUnit is what core.Balance.Credits counts for Tabbit: a percentage of
// the vendor's rolling quota, not a credit count.
const balanceUnit = "%"

// AccountBalance implements core.BalanceProvider for web-token accounts.
func (c *Client) AccountBalance(ctx context.Context, id string, soon time.Duration) (core.Balance, error) {
	if c == nil {
		return core.Balance{}, fmt.Errorf("tabbit: no client")
	}
	key := strings.TrimSpace(id)
	if key == "" {
		return core.Balance{}, fmt.Errorf("tabbit: no account id was given")
	}

	ep, ok := c.lookupEndpoint(key)
	if !ok || epKind(ep) != kindWebToken {
		return core.Balance{}, fmt.Errorf("tabbit: account %q is not a web-token account", key)
	}
	if !ep.Enabled {
		return core.Balance{}, fmt.Errorf("tabbit: account %q is disabled", key)
	}

	wa := c.webAuthFrom(ep.Token, endpointLabel(ep), epOriginPanel, ep.BaseURL)
	usage, err := c.webFetchUsage(ctx, wa)
	if err != nil {
		return core.Balance{}, err
	}
	return quotaBalance(usage), nil
}

// quotaBalance converts the vendor's "used percent" into the panel's remaining
// shape.  The clamp is defensive: the field is a percentage, and a negative or
// above-100 value must not draw a progress bar outside its track.
func quotaBalance(u webUsage) core.Balance {
	used := float64(u.UsagePercentage)
	if math.IsNaN(used) || math.IsInf(used, 0) {
		used = 0
	}
	if used < 0 {
		used = 0
	}
	if used > 100 {
		used = 100
	}
	return core.Balance{
		Credits: int64(math.Round(100 - used)),
		Used:    used,
		Total:   100,
		Unit:    balanceUnit,
	}
}

// Compile-time proof that this module satisfies the optional balance contract.
var _ core.BalanceProvider = (*Client)(nil)
