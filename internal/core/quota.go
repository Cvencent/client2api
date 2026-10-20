package core

import (
	"context"
	"time"
)

// BalanceLine is an additional meter shown beside an account's primary
// balance. Vendors often expose two different entitlement systems (for example
// credits and a rolling usage window), so the primary Balance remains the
// credit number and these rows carry the extra meters without changing its
// meaning.
type BalanceLine struct {
	Label            string
	Unit             string
	Amount           *int64
	Total            *int64
	UsedPercent      *float64
	RemainingPercent *float64
	ResetsAt         string
}

// Balance is one account's live credit position.
//
// It has no JSON tags on purpose: the panel shapes the response (the reference
// answers {ok, credits, credits_total}) and adding tags here would fix a wire
// format in the shared package for everyone.  The extra buckets exist because
// the reference computes them for the pool's "spend the expiring credit first"
// routing, and a module that can compute them is worth more than one that only
// reports the total.
type Balance struct {
	Credits int64   // credits left right now
	Used    float64 // what the vendor reports as consumed; fractional for credit meters
	Total   int64   // the account's capacity
	// Scale converts Credits, Total, Expiring and EarliestRemaining from the
	// module's storage units to the unit shown by the panel. Zero means one.
	// Used is already expressed in display units.
	Scale float64
	// Unverified means Credits is the best conservative number to show, but
	// the vendor's own reply contradicts it, so it must not be treated as a
	// confirmed empty wallet or drive a low-balance park. WorkBuddy's trial
	// package is the observed case: the cycle view says 0 while the stale
	// lifetime view still advertises capacity.
	Unverified bool
	// Unlimited is the vendor's own statement that Credits is not finite.
	// Credits and Total are then advisory and must not be rendered as a
	// running balance; Trae uses credits_limit=-1 for this case.
	Unlimited bool
	Expiring  int64 // the part of Credits that expires before the caller's window ends
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
	// Lines carries additional meters the vendor reports beside Credits. An
	// empty slice means the module only has the primary balance.
	Lines []BalanceLine
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

// InviteCodeResult is one account's answer to an invite-code binding attempt.
//
// A vendor refusal (bad code, usage limit, already bound to a different code)
// is a RESULT, not a Go error: the panel shows it on that account's row while
// the other rows continue.  A Go error is reserved for an attempt that could
// not be made at all (unknown account, no store).
type InviteCodeResult struct {
	AccountID   string `json:"account_id,omitempty"`
	OK          bool   `json:"ok"`
	Code        string `json:"code,omitempty"`
	Activated   bool   `json:"activated,omitempty"`
	Already     bool   `json:"already,omitempty"`
	AppliedCode string `json:"applied_code,omitempty"`
	Reward      int64  `json:"reward,omitempty"`
	Message     string `json:"message,omitempty"`
	Error       string `json:"error,omitempty"`
	At          string `json:"at,omitempty"`
}

// RedemptionResult is one account's answer to a redemption-code attempt.
// Points is what the vendor says this redemption added; Balance is the fresh
// current balance when the vendor reports one.
type RedemptionResult struct {
	AccountID string `json:"account_id,omitempty"`
	OK        bool   `json:"ok"`
	Code      string `json:"code,omitempty"`
	Points    int64  `json:"points,omitempty"`
	Balance   int64  `json:"balance,omitempty"`
	Message   string `json:"message,omitempty"`
	Error     string `json:"error,omitempty"`
	At        string `json:"at,omitempty"`
}

// InviteCodeProvider lets the panel bind an invite code to an account and
// redeem a separate redemption code.  Loomy uses two different endpoints for
// these: the invite code is the "兑换积分" button's activation call, while a
// redemption code is a points voucher.  Keeping both on one optional interface
// gives the panel one capability switch and one place to explain the split.
type InviteCodeProvider interface {
	Client
	// InviteCodeDefault is the account-wide invite code the module will use
	// when the operator does not supply one.  Empty means no default.
	InviteCodeDefault(ctx context.Context) string
	// BindAccountInviteCode binds code to one account.  An empty code selects
	// InviteCodeDefault.  The result reports the vendor's own answer.
	BindAccountInviteCode(ctx context.Context, id, code string) (InviteCodeResult, error)
	// RedeemAccountCode redeems a redemption code (not an invite code) for
	// one account.  The two code namespaces are deliberately separate.
	RedeemAccountCode(ctx context.Context, id, code string) (RedemptionResult, error)
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

// AsInviteCodeProvider narrows a registered client.
func AsInviteCodeProvider(c Client) (InviteCodeProvider, bool) {
	ip, ok := c.(InviteCodeProvider)
	return ip, ok
}

// InviteCodeFleetBinder marks the subset of InviteCodeProvider implementations
// where running the bind across the whole account pool is meaningful and safe.
//
// Loomy qualifies: every account can carry the same invite code and the vendor
// answers "already bound" idempotently.  Trae's code-fission activity does not:
// the vendor only accepts an invitee that has never registered, so a fleet-wide
// sweep would fail on every account that already exists.  The panel hides the
// "bind all" action for a module that does not opt in.
type InviteCodeFleetBinder interface {
	InviteCodeBindAllSafe() bool
}

// AsInviteCodeFleetBinder reports the opt-in without forcing every provider to
// implement it.
func AsInviteCodeFleetBinder(c Client) (InviteCodeFleetBinder, bool) {
	fb, ok := c.(InviteCodeFleetBinder)
	return fb, ok
}
