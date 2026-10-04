package workbuddy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"client2api/internal/core"
)

// panel_quota.go exposes the credit and prize reads the panel serves through
// the optional interfaces internal/core declares.  The panel reaches them via
// core.CapabilitiesOf, which type-asserts the registered client, so adding
// these methods is the whole wiring: nothing is registered here, and no new
// vendor call is invented.  Every read below is one of the module's own,
// already-tested helpers, reached through the same account-id bridge the rest
// of the module uses.
//
// The three assertions below are compile-time proof that the module satisfies
// the panel's optional contracts; if a signature drifts, this file stops
// building instead of silently dropping out of CapabilitiesOf.
var (
	_ core.BalanceProvider = (*Client)(nil)
	_ core.PackageProvider = (*Client)(nil)
	_ core.VoucherProvider = (*Client)(nil)
)

// balanceUnit is what a core.Balance.Credits counts for this vendor.  The panel
// prints the number with this label and never converts between vendors, because
// they do not agree on a unit: this one sells 积分, zcode sells tokens.
const balanceUnit = "积分"

// panelAuth resolves the account id the panel passes (always the id its
// Accounts()/pool records carry) to the live credential this module holds.
//
// An unknown id is a plain error and must not cost an upstream call: the
// lookup is purely local, and the error names the id the caller sent so the
// panel's 404/502 text is actionable.  No token can appear in it by
// construction -- it carries only the caller's own id.
func (c *Client) panelAuth(id string) (*Auth, error) {
	if c == nil {
		return nil, fmt.Errorf("workbuddy: no client")
	}
	key := strings.TrimSpace(id)
	if key == "" {
		return nil, fmt.Errorf("workbuddy: no account id was given")
	}
	// Cheap and offline: refreshAccounts only reloads the credential files when
	// the reload interval has elapsed, so this never becomes an extra request.
	c.refreshAccounts(false)
	if a := c.findAuth(key); a != nil {
		return a, nil
	}
	return nil, fmt.Errorf("workbuddy: account %q not found", key)
}

// AccountBalance implements core.BalanceProvider.  It reports the account's
// credit totals and, when the operator asked for a window, how much of that
// credit is about to expire.
//
// A `soon` that is zero or negative means the operator declined the expiring
// bucket, and it is passed through unchanged: UserResourceDetailedWithExpiry
// treats a non-positive window as "do not compute this bucket", and inventing
// a default here would report a number the operator never asked for.
func (c *Client) AccountBalance(ctx context.Context, id string, soon time.Duration) (core.Balance, error) {
	a, err := c.panelAuth(id)
	if err != nil {
		return core.Balance{}, err
	}
	remain, total, expiring, earliestAt, earliestRemaining, err :=
		c.UserResourceDetailedWithExpiry(ctx, a, soon)
	if err != nil {
		// The vendor's own wording is preserved verbatim; the panel redacts it.
		return core.Balance{}, err
	}
	// The pool is fed here rather than at each caller, because every balance
	// read -- the panel's account view, the scheduler's refresh pass, a login, a
	// credential import -- arrives through this one method.  One hook covers all
	// of them, no future caller can forget it, and the read it uses is already
	// paid for.
	//
	// This is the whole point of the loop: an account parked for having no
	// credit used to stay parked until an operator pressed Revive by hand, even
	// after the vendor granted more.  A positive balance is the evidence that
	// the park no longer applies, so recording it lifts the park.
	c.pool.SetCreditsDetailed(a, remain, total, expiring, earliestAt, earliestRemaining)
	return core.Balance{
		Credits:           remain,
		Total:             total,
		Expiring:          expiring,
		EarliestAt:        earliestAt,
		EarliestRemaining: earliestRemaining,
		// This vendor sells 积分, and both the CN and the intl realms bill in
		// them, so the label is not realm-dependent.
		Unit: balanceUnit,
	}, nil
}

// AccountPackages implements core.PackageProvider.  The module's CreditPackage
// and the panel's core.CreditPackage carry the same fields, so this is a
// field-for-field conversion plus the module's own pre-computed sums -- never a
// re-derivation, which could disagree with CreditPackages' clamping rules.
//
// The breakdown is always a non-nil slice, so an account with no packages
// serialises as [] rather than null.
func (c *Client) AccountPackages(ctx context.Context, id string) (core.PackageReport, error) {
	a, err := c.panelAuth(id)
	if err != nil {
		return core.PackageReport{}, err
	}
	packs, remain, size, err := c.CreditPackages(ctx, a)
	if err != nil {
		return core.PackageReport{}, err
	}
	out := make([]core.CreditPackage, 0, len(packs))
	for _, p := range packs {
		out = append(out, core.CreditPackage{
			Name:           p.Name,
			Remain:         p.Remain,
			Used:           p.Used,
			Size:           p.Size,
			EndTime:        p.EndTime,
			ExpiresAt:      p.ExpiresAt,
			CreatedAt:      p.CreatedAt,
			PackageCode:    p.PackageCode,
			SubProductCode: p.SubProductCode,
			SubProductName: p.SubProductName,
			Cycle:          p.Cycle,
		})
	}
	return core.PackageReport{Remain: remain, Size: size, Packages: out}, nil
}

// AccountVouchers implements core.VoucherProvider.  An account that holds no
// prizes is a fact, not a failure: it returns an empty, non-nil slice and a nil
// error, because the module's SchoolVouchers only reports an error when the
// vendor actually refused.
func (c *Client) AccountVouchers(ctx context.Context, id string) ([]core.Voucher, error) {
	a, err := c.panelAuth(id)
	if err != nil {
		return nil, err
	}
	vs, err := c.SchoolVouchers(ctx, a)
	if err != nil {
		return nil, err
	}
	out := make([]core.Voucher, 0, len(vs))
	for _, v := range vs {
		out = append(out, core.Voucher{
			GrantID:   v.GrantID,
			DrawUUID:  v.DrawUUID,
			SKUCode:   v.SKUCode,
			PrizeName: v.PrizeName,
			Code:      v.Code,
			ValidFrom: v.ValidFrom,
			ValidTo:   v.ValidTo,
			GrantedAt: v.GrantedAt,
		})
	}
	return out, nil
}
