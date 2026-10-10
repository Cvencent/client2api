package qoder

import (
	"context"
	"fmt"
	"math"
	"time"

	"client2api/internal/core"
)

// balance.go implements core.BalanceProvider: the credit position the panel
// shows in the accounts table.
//
// Qoder CN meters a single thing -- credits -- and reports it as two pools:
// `userQuota` is the subscription allowance and `addOnQuota` is the reward
// balance the daily activity fills.  The panel wants one number, so the pools
// are summed; the unit the vendor reports ("credits") is passed through rather
// than renamed.

// balanceUnit is the fallback unit when the vendor does not spell one.
const balanceUnit = "credits"

// AccountBalance implements core.BalanceProvider.
//
// `soon` is ignored on purpose: the reward credits the daily activity grants do
// carry a 30-day life, but the ledger does not report a per-tranche expiry, and
// inventing one would put a made-up deadline in front of the operator.
func (c *Client) AccountBalance(ctx context.Context, id string, soon time.Duration) (core.Balance, error) {
	acc, ok := c.store.lookup(id)
	if !ok {
		return core.Balance{}, fmt.Errorf("qoder: no account %q", id)
	}

	usage, err := c.up.quotaUsage(ctx, acc.Token)
	if err != nil {
		// A balance read is one more piece of evidence about the credential, so
		// a rejection here parks the account -- unless the caller walked away,
		// which says nothing about the credential.  The panel reads balances on
		// every redraw and cancels the read in flight when it does.
		if !callerGone(err) {
			c.penalise(id, err, time.Now().UTC())
		}
		return core.Balance{}, err
	}
	c.store.clearPenalties(id)

	credits := int64(math.Round(usage.UserQuota.Remaining)) + int64(math.Round(usage.AddOnQuota.Remaining))
	total := int64(math.Round(usage.UserQuota.Total)) + int64(math.Round(usage.AddOnQuota.Total))
	used := usage.UserQuota.Used + usage.AddOnQuota.Used
	unit := firstNonEmpty(usage.AddOnQuota.Unit, usage.UserQuota.Unit, balanceUnit)

	return core.Balance{
		Credits: credits,
		Used:    used,
		Total:   total,
		Unit:    unit,
	}, nil
}

// AccountPackages implements core.PackageProvider.  Qoder reports the credit
// ledger as two pools, so those two pools are exactly the two tranches the
// panel shows.  No tranche is invented when the vendor reports a zero pool:
// a zero row still carries the vendor's own total, which is useful when the
// subscription has been fully spent.
func (c *Client) AccountPackages(ctx context.Context, id string) (core.PackageReport, error) {
	acc, ok := c.store.lookup(id)
	if !ok {
		return core.PackageReport{}, fmt.Errorf("qoder: no account %q", id)
	}

	usage, err := c.up.quotaUsage(ctx, acc.Token)
	if err != nil {
		if !callerGone(err) {
			c.penalise(id, err, time.Now().UTC())
		}
		return core.PackageReport{}, err
	}
	c.store.clearPenalties(id)

	user := creditPackage("订阅额度", usage.UserQuota)
	addon := creditPackage("活动加赠", usage.AddOnQuota)
	if expiry := epochMillis(usage.ExpiresAt); !expiry.IsZero() {
		user.EndTime = expiry.UTC().Format(time.RFC3339)
	}

	return core.PackageReport{
		Remain:   user.Remain + addon.Remain,
		Size:     user.Size + addon.Size,
		Packages: []core.CreditPackage{user, addon},
	}, nil
}

// creditPackage converts one vendor pool into a panel tranche, keeping the
// vendor's own numbers.  A negative remainder is clamped to zero; that is a
// display guard, not a rewrite of the vendor's total.
func creditPackage(name string, b quotaBucket) core.CreditPackage {
	remain := int64(math.Round(b.Remaining))
	if remain < 0 {
		remain = 0
	}
	size := int64(math.Round(b.Total))
	used := int64(math.Round(b.Used))
	if used < 0 {
		used = 0
	}
	if size < remain {
		size = remain
	}
	return core.CreditPackage{Name: name, Remain: remain, Used: used, Size: size}
}

// epochMillis turns a vendor millisecond timestamp into a Time, treating an
// absent or nonsensical value as "no deadline".
func epochMillis(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
