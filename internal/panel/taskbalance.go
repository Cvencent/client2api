package panel

import (
	"context"
	"strings"
	"time"

	"client2api/internal/core"
)

const taskBalanceRefreshTimeout = 30 * time.Second

// refreshBalanceAfterTask keeps the account-pool balance in step with a chore
// that just changed vendor-side credit.  The balance read is best effort: it
// must never turn an otherwise successful task into a failed one.
func (p *panel) refreshBalanceAfterTask(c core.Client, accountID string) {
	if p == nil || p.balanceCache == nil || c == nil {
		return
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return
	}
	bp, ok := core.AsBalanceProvider(c)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), taskBalanceRefreshTimeout)
	defer cancel()
	bal, err := bp.AccountBalance(ctx, accountID, p.expiringSoon())
	if err != nil {
		return
	}
	p.balanceCache.put(c.Name(), accountID, bal)
}
