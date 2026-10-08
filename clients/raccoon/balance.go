package raccoon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"client2api/internal/core"
)

// The credits endpoints are READ-ONLY on purpose: this is a high-frequency
// panel path and the only write endpoint the vendor has for points
// (desktop/v1/login/points/grant) is a one-off reward that must never be
// fired from a status refresh.
//
// The daily 300 login credits are claimed by the `login-points` check-in
// action (POST /api/web/desktop/v1/login/points/grant, see checkin.go).
// That one-off write must never be fired from a status refresh, so nothing
// here tries to claim them.

// balanceAccount resolves the account a credits call should use. An empty id
// means "any usable account".
func (c *Client) balanceAccount(id string) (*entry, error) {
	if strings.TrimSpace(id) != "" {
		e := c.pool.find(id)
		if e == nil {
			return nil, fmt.Errorf("raccoon: unknown account %q", id)
		}
		return e, nil
	}
	if e := c.pool.pick(nil); e != nil {
		return e, nil
	}
	if n := c.pool.len(); n > 0 {
		return nil, fmt.Errorf("raccoon: no account available for credits (%s)", c.pool.summary())
	}
	return nil, core.ErrNotConfigured
}

// AccountBalance implements core.BalanceProvider. `soon` is ignored: the
// vendor reports no expiry for points, so there is nothing to warn about.
//
// If the response carries no `available_points` field the module reports an
// error instead of a fabricated zero — an absent field means "we could not
// read it", not "you have nothing".
func (c *Client) AccountBalance(ctx context.Context, id string, soon time.Duration) (core.Balance, error) {
	e, err := c.balanceAccount(id)
	if err != nil {
		return core.Balance{}, err
	}
	ctx, cancel := c.deadline(ctx)
	defer cancel()
	data, err := c.fetchBalance(ctx, e.acct.cred())
	if err != nil {
		return core.Balance{}, err
	}
	if data.AvailablePoints == nil {
		return core.Balance{}, errors.New("raccoon: balance response carried no available_points")
	}
	avail := int64(*data.AvailablePoints)
	return core.Balance{Credits: avail, Total: avail, Unit: "积分"}, nil
}

// AccountPackages implements core.PackageProvider, exposing the credit pools
// the vendor reports separately. A pool that is absent or zero is omitted
// rather than shown as an empty row.
func (c *Client) AccountPackages(ctx context.Context, id string) (core.PackageReport, error) {
	e, err := c.balanceAccount(id)
	if err != nil {
		return core.PackageReport{}, err
	}
	ctx, cancel := c.deadline(ctx)
	defer cancel()
	data, err := c.fetchBalance(ctx, e.acct.cred())
	if err != nil {
		return core.PackageReport{}, err
	}
	if data.AvailablePoints == nil {
		return core.PackageReport{}, errors.New("raccoon: balance response carried no available_points")
	}
	pools := []struct {
		name string
		v    *flexFloat
	}{
		{"奖励积分", data.RewardPoints},
		{"每日积分", data.DailyPoints},
		{"会员积分", data.MonthlyPoints},
		{"充值积分", data.TopupPoints},
	}
	rep := core.PackageReport{}
	seen := false
	for _, p := range pools {
		if p.v == nil || *p.v <= 0 {
			continue
		}
		seen = true
		n := int64(*p.v)
		rep.Packages = append(rep.Packages, core.CreditPackage{
			Name:   p.name,
			Remain: n,
			Size:   n,
		})
	}
	if !seen {
		// The server reported only a total: one honest package.
		n := int64(*data.AvailablePoints)
		rep.Packages = append(rep.Packages, core.CreditPackage{Name: "可用积分", Remain: n, Size: n})
	}
	for _, p := range rep.Packages {
		rep.Remain += p.Remain
		rep.Size += p.Size
	}
	return rep, nil
}
