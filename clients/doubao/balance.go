package doubao

import (
	"context"
	"fmt"
	"time"

	"client2api/internal/core"
)

// balance.go reads the account's credit position.  Doubao reports credits
// ("积分") through the commerce benefit-supply endpoints; the module surfaces
// the total as a Balance the panel renders with the "积分" unit.

// creditReply is the union of the shapes the credit endpoint has returned.  The
// module takes the first non-zero number it can find rather than pinning one
// field name, because the vendor's envelope changes between builds.
type creditReply struct {
	Data struct {
		CreditNum      *float64 `json:"credit_num"`
		CreditTotal    *float64 `json:"credit_total"`
		TotalCredit    *float64 `json:"total_credit"`
		RemainCredit   *float64 `json:"remain_credit"`
		Available      *float64 `json:"available_credit"`
		CreditUsageNum *float64 `json:"credit_usage_num"`
	} `json:"data"`
	CreditNum      *float64 `json:"credit_num"`
	CreditTotal    *float64 `json:"credit_total"`
	TotalCredit    *float64 `json:"total_credit"`
	RemainCredit   *float64 `json:"remain_credit"`
	CreditUsageNum *float64 `json:"credit_usage_num"`
}

// firstNumber returns the first non-nil pointer's value.
func firstNumber(vals ...*float64) (int64, bool) {
	for _, v := range vals {
		if v != nil {
			return int64(*v), true
		}
	}
	return 0, false
}

// AccountBalance implements core.BalanceProvider.
func (c *Client) AccountBalance(ctx context.Context, id string, soon time.Duration) (core.Balance, error) {
	acc, ok := c.store.byID(id)
	if !ok {
		return core.Balance{}, fmt.Errorf("doubao: unknown account %q", id)
	}
	cctx, cancel := context.WithTimeout(ctx, c.cfg.probeTimeout())
	defer cancel()

	var reply creditReply
	if err := c.up.getJSON(cctx, &acc, pathCreditNum, nil, &reply); err != nil {
		c.noteFailure(err)
		return core.Balance{}, err
	}
	remain, okRemain := firstNumber(
		reply.Data.CreditNum, reply.Data.RemainCredit, reply.Data.Available,
		reply.CreditNum, reply.RemainCredit,
	)
	if !okRemain {
		remain, _ = firstNumber(reply.Data.CreditTotal, reply.Data.TotalCredit, reply.CreditTotal, reply.TotalCredit)
	}
	b := core.Balance{
		Credits: remain,
		Unit:    "积分",
	}
	if used, ok := firstNumber(reply.Data.CreditUsageNum, reply.CreditUsageNum); ok {
		b.Used = float64(used)
	}
	if total, ok := firstNumber(reply.Data.CreditTotal, reply.Data.TotalCredit, reply.CreditTotal, reply.TotalCredit); ok {
		b.Total = total
	}
	return b, nil
}

// creditHistoryRow is one row of the usage history; not used for Balance but
// kept for a future panel surface.
type creditHistoryRow struct {
	CreditNum float64 `json:"credit_num"`
	Timestamp int64   `json:"timestamp"`
	Reason    string  `json:"reason"`
}
