package autoclaw

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"client2api/internal/core"
)

// AutoClaw credits are read through the identity gateway's asset manager:
//
//   - v2 /agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw returns the
//     displayable wallets the account currently holds and the authoritative
//     total_balance;
//   - v1 /agent-assetmgr/api/v1/wallet-instances is the fallback.  It is the
//     only shape that breaks the total down by scope (daily / monthly /
//     long_term / campaign), which is why both are kept;
//   - /agent-assetmgr/api/v1/ledgers_std is the read-only ledger page.
//
// Nothing here writes: the only write endpoint, the daily claim, lives in
// checkin.go and is fired deliberately, never from a status refresh.

const balanceTimeout = 20 * time.Second

// walletScope is one v1 wallet instance's scope, normalised the way the
// desktop client normalises it (`wallet_scope_daily` -> `daily`).
func normalizeWalletScope(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	return strings.TrimPrefix(s, "wallet_scope_")
}

// balanceAccount resolves the account a credits call should use. An empty id
// means "any usable account".
func (c *Client) balanceAccount(id string) (*entry, error) {
	if strings.TrimSpace(id) != "" {
		e := c.pool.find(id)
		if e == nil {
			return nil, fmt.Errorf("autoclaw: unknown account %q", id)
		}
		return e, nil
	}
	if e := c.pool.pick(nil); e != nil {
		return e, nil
	}
	if n := c.pool.len(); n > 0 {
		return nil, fmt.Errorf("autoclaw: no account available for credits (%s)", c.pool.summary())
	}
	return nil, core.ErrNotConfigured
}

// walletLine is one displayable wallet (v2) or wallet instance (v1).
type walletLine struct {
	Name    string
	Scope   string
	Balance int64
	View    string
}

// creditsSnapshot is the decoded credits read.  Total is the gateway's own
// total_balance when it reported one; otherwise it is the sum of the scopes.
type creditsSnapshot struct {
	Total   int64
	Wallets []walletLine
	ByScope map[string]int64
	Source  string
}

func (s creditsSnapshot) scopeTotal(scope string) int64 {
	if s.ByScope == nil {
		return 0
	}
	return s.ByScope[scope]
}

// fetchCredits reads v2 first and only falls back to v1 when v2 could not
// answer at all, mirroring the desktop adapter exactly.
func (c *Client) fetchCredits(ctx context.Context, cred credential) (creditsSnapshot, error) {
	snap, err := c.fetchWalletsV2(ctx, cred)
	if err == nil {
		return snap, nil
	}
	v1, verr := c.fetchWalletsV1(ctx, cred)
	if verr == nil {
		return v1, nil
	}
	return creditsSnapshot{}, err
}

// fetchCreditsWithRefresh reads the credits snapshot, renewing once and
// retrying exactly once when the server rejects the access token.
//
// The retry lives OUTSIDE fetchCredits on purpose: fetchCredits falls back
// from v2 to v1, and that fallback swallows the v2 refusal (it returns the v2
// error) - so an auth rejection has to be judged on the whole read, not on a
// single attempt, or a stale token would be reported as a permanent failure
// until the operator happened to run a chat/test that renewed it.
func (c *Client) fetchCreditsWithRefresh(ctx context.Context, e *entry) (creditsSnapshot, error) {
	snap, err := c.fetchCredits(ctx, e.acct.cred())
	if err == nil || !isAuthError(err) || !e.acct.cred().refreshable() {
		return snap, err
	}
	if rerr := c.refreshAccount(ctx, e); rerr != nil {
		return snap, err
	}
	return c.fetchCredits(ctx, e.acct.cred())
}

func (c *Client) fetchWalletsV2(ctx context.Context, cred credential) (creditsSnapshot, error) {
	ctx, cancel := withTimeout(ctx, balanceTimeout)
	defer cancel()
	q := url.Values{"biz_app_id": []string{"autoclaw"}}
	var data struct {
		TotalBalance flexFloat `json:"total_balance"`
		Wallets      []struct {
			PublicWalletType string    `json:"public_wallet_type"`
			DisplayName      string    `json:"display_name"`
			Balance          flexFloat `json:"balance"`
			Display          bool      `json:"display"`
			Priority         flexFloat `json:"priority"`
			BalanceView      string    `json:"balance_view"`
		} `json:"wallets"`
	}
	if err := c.do(ctx, "GET", pathWalletsV2+"?"+q.Encode(), identityHeaders(cred, c.cfg.version()), nil, &data); err != nil {
		return creditsSnapshot{}, err
	}
	snap := creditsSnapshot{Total: int64(data.TotalBalance), ByScope: map[string]int64{}, Source: "wallets-v2"}
	for _, w := range data.Wallets {
		if !w.Display {
			continue
		}
		name := firstNonEmpty(strings.TrimSpace(w.DisplayName), strings.TrimSpace(w.PublicWalletType))
		if name == "" {
			continue
		}
		snap.Wallets = append(snap.Wallets, walletLine{
			Name:    name,
			Scope:   normalizeWalletScope(w.PublicWalletType),
			Balance: int64(w.Balance),
			View:    strings.TrimSpace(w.BalanceView),
		})
	}
	return snap, nil
}

func (c *Client) fetchWalletsV1(ctx context.Context, cred credential) (creditsSnapshot, error) {
	ctx, cancel := withTimeout(ctx, balanceTimeout)
	defer cancel()
	q := url.Values{"wallet_type": []string{"all"}, "wallet_scope": []string{"all"}}
	var data struct {
		TotalBalance    flexFloat `json:"total_balance"`
		WalletInstances []struct {
			Status      string    `json:"status"`
			Display     bool      `json:"display"`
			WalletScope string    `json:"wallet_scope"`
			Balance     flexFloat `json:"balance"`
		} `json:"wallet_instances"`
	}
	if err := c.do(ctx, "GET", pathWalletInstancesV1+"?"+q.Encode(), identityHeaders(cred, c.cfg.version()), nil, &data); err != nil {
		return creditsSnapshot{}, err
	}
	snap := creditsSnapshot{Total: int64(data.TotalBalance), ByScope: map[string]int64{}, Source: "wallet-instances-v1"}
	for _, inst := range data.WalletInstances {
		if !strings.EqualFold(strings.TrimSpace(inst.Status), "active") || !inst.Display {
			continue
		}
		scope := normalizeWalletScope(inst.WalletScope)
		snap.ByScope[scope] += int64(inst.Balance)
	}
	return snap, nil
}

// AccountBalance implements core.BalanceProvider.
func (c *Client) AccountBalance(ctx context.Context, id string, soon time.Duration) (core.Balance, error) {
	e, err := c.balanceAccount(id)
	if err != nil {
		return core.Balance{}, err
	}
	c.ensureFresh(ctx, e)
	snap, err := c.fetchCreditsWithRefresh(ctx, e)
	if err != nil {
		c.noteFailure(err)
		return core.Balance{}, err
	}
	c.noteUpstreamOK()
	total := snap.Total
	if total == 0 && len(snap.ByScope) > 0 {
		for _, v := range snap.ByScope {
			total += v
		}
	}
	if total == 0 {
		for _, w := range snap.Wallets {
			total += w.Balance
		}
	}
	bal := core.Balance{Credits: total, Total: total, Unit: "积分"}
	bal.Lines = walletBalanceLines(snap.Wallets)
	return bal, nil
}

// walletBalanceLines lists the vendor's displayable wallets beside the summed
// credit total. AutoClaw's own credits detail renders exactly these wallets, so
// the panel mirrors the client instead of collapsing everything into one
// number: a non-credit wallet the vendor reports (for example a token grant)
// shows up here on its own, and the gateway never invents a balance the vendor
// did not send.
func walletBalanceLines(wallets []walletLine) []core.BalanceLine {
	if len(wallets) == 0 {
		return nil
	}
	out := make([]core.BalanceLine, 0, len(wallets))
	for _, w := range wallets {
		amount := w.Balance
		out = append(out, core.BalanceLine{
			Label:  w.Name,
			Unit:   walletUnit(w.Scope),
			Amount: &amount,
		})
	}
	return out
}

// walletUnit labels the wallet number only when the vendor's scope identifies
// it as a credit pool; an unknown wallet (a token grant, say) keeps its own
// display name and no invented unit.
func walletUnit(scope string) string {
	switch scope {
	case "daily", "monthly", "long_term", "annual", "campaign":
		return "积分"
	}
	return ""
}

// AccountPackages implements core.PackageProvider, exposing the credit pools
// the vendor reports.
//
// v2 reports one displayable wallet per grant and is preferred because the
// per-wallet name is what the AutoClaw UI renders.  v1 only reports scopes, so
// each non-zero scope becomes one row, labelled by scope.  A pool that is
// absent or zero is omitted rather than shown as an empty row.
func (c *Client) AccountPackages(ctx context.Context, id string) (core.PackageReport, error) {
	e, err := c.balanceAccount(id)
	if err != nil {
		return core.PackageReport{}, err
	}
	c.ensureFresh(ctx, e)
	snap, err := c.fetchCreditsWithRefresh(ctx, e)
	if err != nil {
		c.noteFailure(err)
		return core.PackageReport{}, err
	}
	c.noteUpstreamOK()
	rep := core.PackageReport{}
	if len(snap.Wallets) > 0 {
		sort.SliceStable(snap.Wallets, func(i, j int) bool { return snap.Wallets[i].Balance > snap.Wallets[j].Balance })
		for _, w := range snap.Wallets {
			if w.Balance <= 0 {
				continue
			}
			rep.Packages = append(rep.Packages, core.CreditPackage{Name: w.Name, Remain: w.Balance, Size: w.Balance, SubProductName: w.Scope})
		}
	}
	if len(rep.Packages) == 0 {
		names := []struct {
			scope string
			label string
		}{
			{"daily", "每日积分"},
			{"monthly", "每月积分"},
			{"long_term", "长期积分"},
			{"annual", "年度积分"},
			{"campaign", "活动积分"},
		}
		seen := map[string]bool{}
		for _, n := range names {
			if seen[n.scope] {
				continue
			}
			seen[n.scope] = true
			v := snap.scopeTotal(n.scope)
			if v <= 0 {
				continue
			}
			rep.Packages = append(rep.Packages, core.CreditPackage{Name: n.label, Remain: v, Size: v})
		}
		for scope, v := range snap.ByScope {
			if v <= 0 || seen[scope] {
				continue
			}
			seen[scope] = true
			rep.Packages = append(rep.Packages, core.CreditPackage{Name: firstNonEmpty(scope, "其他积分"), Remain: v, Size: v})
		}
	}
	if len(rep.Packages) == 0 {
		return core.PackageReport{}, errors.New("autoclaw: credits response carried no displayable wallet")
	}
	for _, p := range rep.Packages {
		rep.Remain += p.Remain
		rep.Size += p.Size
	}
	return rep, nil
}
