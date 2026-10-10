package kimi

import (
	"client2api/internal/core"
)

// poolhealth.go adds the two pool-facing optional capabilities kimi genuinely
// has: a servability verdict and a live in-flight count.
//
// kimi is not a multi-account pool like the coding agents -- it has at most two
// serving identities (the panel login and the CLI login) -- so there is no
// per-account ceiling to report and no sticky-session table beyond the small
// affinity table the two paths already share.  Both numbers that ARE reported
// are real: the in-flight count is fed by the two success paths in Chat, and the
// servability verdict is computed from the same rows Status() renders, so the
// badge and the account table can never disagree.

var (
	_ core.HealthProvider    = (*Client)(nil)
	_ core.PoolStatsReporter = (*Client)(nil)
)

// Health implements core.HealthProvider: "can this module serve a request right
// now, and if not, why not".
//
// The census runs over the same credential rows Status() publishes and applies
// the same usability rule, so a row that reads usable and a Health that reads
// unservable cannot coexist.
func (c *Client) Health() core.Health {
	if c == nil {
		return core.Health{}
	}
	rows := c.statusAccountRows()
	h := core.Health{Total: len(rows)}
	for i := range rows {
		switch {
		case !rows[i].Enabled:
			h.Disabled++
		case !accountRowUsable(rows[i]):
			// A cooling park heals by time and belongs with cooling; every
			// other not-usable state (a dead login, a lapsed token with no
			// refresh) is a fault the operator has to act on.
			if rows[i].State == healthCooling {
				h.Cooling++
			} else {
				h.Disabled++
			}
		default:
			h.Ready++
		}
	}
	switch {
	case h.Total == 0:
		h.Note = "还没有账号；请先在账号页添加 Kimi 账号"
	case h.Ready == 0:
		h.Note = "当前没有可用账号；请查看账号冷却或登录状态"
	default:
		h.Servable = true
	}
	return h
}

// statusAccountRows assembles the credential rows Status() renders, so Health
// can be computed from one source.  It performs exactly the same cached, local
// probes Status() does -- a binary lookup and a credential scan, both cached --
// and folds in the operator's explicit panel choices.
func (c *Client) statusAccountRows() []core.AccountStatus {
	bin, binErr := c.run.binaryPath()
	creds := c.credentials()
	rows := creds.accounts(bin, binErr, c.cfg)
	if acct, ok := c.tokenStatus(); ok {
		rows = append([]core.AccountStatus{acct}, rows...)
	}
	c.applyAccountOverrides(rows)
	return rows
}

// PoolStats implements core.PoolStatsReporter.  kimi has no per-account pool,
// so the only number it can honestly report is the in-flight count; the sticky
// count is the affinity table the two serving paths write through
// bindConversation.
func (c *Client) PoolStats() core.PoolStats {
	if c == nil {
		return core.PoolStats{}
	}
	stats := core.PoolStats{InFlight: int(c.inFlight.Load())}
	if c.affinity != nil {
		stats.StickySessions = c.affinity.Count()
	}
	return stats
}
