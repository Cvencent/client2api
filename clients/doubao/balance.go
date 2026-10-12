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
		CreditInfo struct {
			TotalCreditNum *float64 `json:"total_credit_num"`
			CreditText     string   `json:"credit_text"`
		} `json:"credit_info"`
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

// subscriptionQuotaReply is the shared envelope of the subscription summary
// and overview endpoints. Only the rolling window meter is relevant here.
type subscriptionQuotaReply struct {
	Data struct {
		WindowLimitSection struct {
			WindowLimitGroups []struct {
				WindowLimits []struct {
					WindowType         int      `json:"window_type"`
					UsedPercent        *float64 `json:"used_percent"`
					StartTime          int64    `json:"start_time"`
					EndTime            int64    `json:"end_time"`
					LessThanOnePercent bool     `json:"less_than_one_percent"`
				} `json:"window_limits"`
			} `json:"window_limit_groups"`
		} `json:"window_limit_section"`
	} `json:"data"`
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
	if err := c.up.postJSON(cctx, &acc, pathCreditNum, map[string]any{"need_tasks": false}, &reply); err != nil {
		c.noteFailure(err)
		return core.Balance{}, err
	}
	remain, okRemain := firstNumber(
		reply.Data.CreditInfo.TotalCreditNum,
		reply.Data.CreditNum, reply.Data.RemainCredit, reply.Data.Available,
		reply.CreditNum, reply.RemainCredit,
	)
	if !okRemain {
		remain, _ = firstNumber(reply.Data.CreditTotal, reply.Data.TotalCredit, reply.CreditTotal, reply.TotalCredit)
	}
	b := core.Balance{
		Credits: remain,
		Unit:    firstNonEmpty(reply.Data.CreditInfo.CreditText, "积分"),
	}
	if used, ok := firstNumber(reply.Data.CreditUsageNum, reply.CreditUsageNum); ok {
		b.Used = float64(used)
	}
	if total, ok := firstNumber(reply.Data.CreditTotal, reply.Data.TotalCredit, reply.CreditTotal, reply.TotalCredit); ok {
		b.Total = total
	}
	b.Lines = append(b.Lines, c.membershipQuotaLines(cctx, &acc)...)
	return b, nil
}

// membershipQuotaLines reads the package meters shown beside the credit
// balance. Quota failures are deliberately ignored: the credit balance is the
// primary result and must not disappear when this optional endpoint is
// unavailable.
func (c *Client) membershipQuotaLines(ctx context.Context, acc *account) []core.BalanceLine {
	for _, path := range []string{pathSubscriptionQuotaSummary, pathSubscriptionOverview} {
		var reply subscriptionQuotaReply
		if err := c.up.postJSON(ctx, acc, path, map[string]any{"product_line": "membership"}, &reply); err != nil {
			continue
		}
		if lines := parseMembershipQuotaLines(reply); len(lines) > 0 {
			return lines
		}
	}
	return nil
}

func parseMembershipQuotaLines(reply subscriptionQuotaReply) []core.BalanceLine {
	var out []core.BalanceLine
	seen := map[string]bool{}
	for _, group := range reply.Data.WindowLimitSection.WindowLimitGroups {
		for _, window := range group.WindowLimits {
			if window.UsedPercent == nil {
				continue
			}
			label := ""
			switch window.WindowType {
			case 1:
				// A zero fixed window with no dates is the vendor's empty
				// placeholder, not a package the client displays.
				if *window.UsedPercent == 0 && window.StartTime == 0 && window.EndTime == 0 && !window.LessThanOnePercent {
					continue
				}
				label = "固定额度"
			case 2:
				label = "循环额度"
			default:
				continue
			}
			if seen[label] {
				continue
			}
			seen[label] = true
			used := *window.UsedPercent
			remaining := 100 - used
			if remaining < 0 {
				remaining = 0
			}
			if remaining > 100 {
				remaining = 100
			}
			usedCopy, remainingCopy := used, remaining
			line := core.BalanceLine{
				Label:            label,
				UsedPercent:      &usedCopy,
				RemainingPercent: &remainingCopy,
			}
			if window.EndTime > 0 {
				line.ResetsAt = time.UnixMilli(window.EndTime).In(time.FixedZone("UTC+8", 8*60*60)).Format(time.RFC3339)
			}
			out = append(out, line)
		}
	}
	return out
}

// creditHistoryRow is one row of the usage history; not used for Balance but
// kept for a future panel surface.
type creditHistoryRow struct {
	CreditNum float64 `json:"credit_num"`
	Timestamp int64   `json:"timestamp"`
	Reason    string  `json:"reason"`
}
