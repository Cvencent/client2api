package core

import (
	"context"
	"time"
)

// Balance is one account's live credit position.
//
// It has no JSON tags on purpose: the panel shapes the response (the reference
// answers {ok, credits, credits_total}) and adding tags here would fix a wire
// format in the shared package for everyone.  The extra buckets exist because
// the reference computes them for the pool's "spend the expiring credit first"
// routing, and a module that can compute them is worth more than one that only
// reports the total.
type Balance struct {
	Credits  int64   // credits left right now
	Used     float64 // what the vendor reports as consumed; fractional for credit meters
	Total    int64   // the account's capacity
	Expiring int64   // the part of Credits that expires before the caller's window ends
	// EarliestAt is when the soonest-expiring tranche ends, zero when unknown.
	EarliestAt time.Time
	// EarliestRemaining is what is left in that tranche.
	EarliestRemaining int64
	// Unit names what Credits counts, in the module's own words ("积分",
	// "tokens", "5 小时额度").  The panel renders the number with this label
	// and never converts, because the vendors disagree: a credit vendor sells
	// points, zcode sells time-boxed token grants, and tabbit reports a
	// percentage of a quota.  An empty Unit means the module did not say, and
	// the panel then shows a bare number rather than inventing a name.
	Unit string
}

// CreditPackage is one tranche of an account's credits.
//
// The fields mirror what the vendors actually return, not a normalised model:
// a package is identified by its code as much as by its name (the same name can
// come from two different campaigns), and the timestamps are the only way to
// tell a signup gift from a reward.  Modules that have no such breakdown simply
// do not implement PackageProvider.
type CreditPackage struct {
	Name           string `json:"name"`
	Remain         int64  `json:"remain"`
	Used           int64  `json:"used"`
	Size           int64  `json:"size"`
	EndTime        string `json:"end_time,omitempty"`
	ExpiresAt      int64  `json:"expires_at,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	PackageCode    string `json:"package_code,omitempty"`
	SubProductCode string `json:"sub_product_code,omitempty"`
	SubProductName string `json:"sub_product_name,omitempty"`
	Cycle          bool   `json:"cycle,omitempty"`
}

// PackageReport is one account's package breakdown plus the summed totals the
// dashboard sorts by.
type PackageReport struct {
	Remain   int64           `json:"remain"`
	Size     int64           `json:"size"`
	Packages []CreditPackage `json:"packages"`
}

// Voucher is one prize an account has won in a vendor's activity portal.  Code
// is the redemption code, which is the whole reason the panel lists these.
type Voucher struct {
	GrantID   int64  `json:"grant_id"`
	DrawUUID  string `json:"draw_uuid,omitempty"`
	SKUCode   string `json:"sku_code,omitempty"`
	PrizeName string `json:"prize_name,omitempty"`
	Code      string `json:"code"`
	ValidFrom string `json:"valid_from,omitempty"`
	ValidTo   string `json:"valid_to,omitempty"`
	GrantedAt string `json:"granted_at,omitempty"`
}

// BalanceProvider lets the panel ask one account what it has left.
//
// It takes a panel account id, not the module's own credential type, because
// the panel must never see a credential.  soon is the operator's "expiring
// soon" window: a module should put the credits that expire inside it into
// Balance.Expiring, and pass 0 to mean "do not compute that bucket".
//
// Unlike Checkin, a vendor error here is an error: the panel has nothing
// sensible to display without a number, so it answers 502 rather than 200.
type BalanceProvider interface {
	Client
	AccountBalance(ctx context.Context, id string, soon time.Duration) (Balance, error)
}

// PackageProvider lets the panel show the per-tranche credit breakdown.  It is
// separate from BalanceProvider because a vendor can offer one without the
// other: a single pooled balance has no packages, and a package list implies a
// balance.
type PackageProvider interface {
	Client
	// AccountPackages reports one account's tranches.  A refusal by the vendor
	// is an error; the panel shows it per row and keeps the other accounts.
	AccountPackages(ctx context.Context, id string) (PackageReport, error)
}

// VoucherProvider lets the panel list the redemption codes an account holds.
// An empty list is a fact, not an error: most accounts have won nothing.
type VoucherProvider interface {
	Client
	AccountVouchers(ctx context.Context, id string) ([]Voucher, error)
}

// AsBalanceProvider narrows a registered client.
func AsBalanceProvider(c Client) (BalanceProvider, bool) {
	bp, ok := c.(BalanceProvider)
	return bp, ok
}

// AsPackageProvider narrows a registered client.
func AsPackageProvider(c Client) (PackageProvider, bool) {
	pp, ok := c.(PackageProvider)
	return pp, ok
}

// AsVoucherProvider narrows a registered client.
func AsVoucherProvider(c Client) (VoucherProvider, bool) {
	vp, ok := c.(VoucherProvider)
	return vp, ok
}
