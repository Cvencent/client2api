package workbuddy

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"client2api/internal/core"
)

// The consecutive-login bonus pass: mend yesterday, collect the starter gift and
// any compensation, redeem every unlocked streak tier, then spend every lottery
// chance the redemption just granted.
//
// Ported from the reference internal/scheduler/streak.go (MIT).  The reference
// hangs the pass off its check-in run ("由签到排程 RunCheckinNow 末尾调用"), and
// this module keeps that placement: it is the tail of runScheduledCheckin, not a
// batch of its own.  The vendor unlocks the 7d/14d/28d tiers by *consecutive*
// login days, so a tier only becomes redeemable on the day a run reaches it --
// running the pass anywhere the check-in does not also run would simply miss
// that day.
//
// Every step is idempotent and every refusal is expected: an unredeemed tier
// answers 403 "not enough consecutive days", an already-claimed gift is a
// business code.  Both mean "skip me today", never "the account is broken", so
// nothing here touches the pool, and nothing here can turn a credited check-in
// into a reported failure.

// streakDrawGap spaces out the repeated lottery draws.  The reference fires them
// back to back; a few hundred milliseconds costs nothing and keeps a run of
// POSTs from looking like a burst.
const streakDrawGap = 300 * time.Millisecond

// streakBonus runs the whole pass for one account and folds whatever actually
// landed into res.  It returns res unchanged when the account has nothing to
// collect or the realm has no growth programme at all.
func (c *Client) streakBonus(ctx context.Context, a *Auth, res core.TaskResult) core.TaskResult {
	if a.IsGlobal() {
		// D4 gate: the international realm has no CN growth programme, so the
		// pass makes no upstream call at all rather than discovering that 404
		// by asking.
		return res
	}

	c.makeupYesterday(ctx, a)

	// The gift and the compensation are one-shot per account; a repeat is a
	// business-code refusal, which is exactly what the err != nil branch is.
	if credit, err := c.ClaimGift(ctx, a); err == nil && credit > 0 {
		res.Credit += credit
		c.logf("workbuddy: account %s claimed the starter gift (+%d credit)", core.MaskSecret(a.ID()), credit)
	}
	if credit, err := c.ClaimCompensation(ctx, a); err == nil && credit > 0 {
		res.Credit += credit
		c.logf("workbuddy: account %s claimed an activity compensation (+%d credit)", core.MaskSecret(a.ID()), credit)
	}

	full, err := c.GrowthStreakFull(ctx, a)
	if err != nil {
		c.logf("workbuddy: account %s streak read failed: %v", core.MaskSecret(a.ID()), describeFailure(err))
		return res
	}
	// The tier list carries the rewards; the three status strings say whether
	// the tier is open.  Both are needed: a tier present in the list with
	// status "locked" must not be attempted.
	statuses := map[string]string{
		"7d":  full.RedemptionStatus.Tier7dStatus,
		"14d": full.RedemptionStatus.Tier14dStatus,
		"28d": full.RedemptionStatus.Tier28dStatus,
	}
	for _, tier := range full.RedemptionStatus.Tiers {
		if ctx.Err() != nil {
			break
		}
		switch strings.ToLower(strings.TrimSpace(statuses[tier.Tier])) {
		case "locked", "claimed":
			continue
		}
		if err := c.GrowthRedeemTier(ctx, a, tier.Tier); err != nil {
			// Not unlocked yet is the ordinary state of a tier before its day
			// arrives, so this is a note, not an alarm.
			c.logf("workbuddy: account %s redeem %s: %v", core.MaskSecret(a.ID()), tier.Tier, describeFailure(err))
			continue
		}
		res.Credit += int64(tier.Credit)
		res.Energy += int64(tier.Energy)
		c.logf("workbuddy: account %s redeemed tier %s (+%d credit, +%d energy, %d card(s), %d draw(s))",
			core.MaskSecret(a.ID()), tier.Tier, tier.Credit, tier.Energy, tier.Cards, tier.Chances)
	}

	// Draw last: the chances a redemption just granted are already counted by
	// the vendor, so the summary has to be read after the redemptions.
	chances, err := c.LotteryChances(ctx, a)
	if err != nil {
		c.logf("workbuddy: account %s lottery summary failed: %v", core.MaskSecret(a.ID()), describeFailure(err))
		return res
	}
	drawn := 0
	for i := 0; i < chances; i++ {
		if ctx.Err() != nil {
			break
		}
		raw, err := c.LotteryDraw(ctx, a)
		if err != nil {
			c.logf("workbuddy: account %s draw %d failed: %v", core.MaskSecret(a.ID()), i+1, describeFailure(err))
			break
		}
		drawn++
		c.logf("workbuddy: account %s draw %d: %s", core.MaskSecret(a.ID()), i+1, compactPrizePayload(raw))
		if i+1 < chances && !sleepCtx(ctx, streakDrawGap) {
			break
		}
	}
	if drawn > 0 {
		c.logf("workbuddy: account %s spent %d lottery chance(s)", core.MaskSecret(a.ID()), drawn)
	}
	return res
}

// makeupYesterday spends a makeup card on yesterday when the run would otherwise
// be broken.  A streak that lapses costs seven days to rebuild, so the card is
// always worth more than the day it saves.  No card, nothing missed and a failed
// read are all silent: none of them is the check-in's problem.
func (c *Client) makeupYesterday(ctx context.Context, a *Auth) {
	missed, err := c.HeatmapYesterdayMissed(ctx, a)
	if err != nil || !missed {
		return
	}
	full, err := c.GrowthStreakFull(ctx, a)
	if err != nil || full.MakeupCards.Balance <= 0 {
		return
	}
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	if err := c.UseMakeupCard(ctx, a, yesterday); err != nil {
		c.logf("workbuddy: account %s makeup %s failed: %v", core.MaskSecret(a.ID()), yesterday, describeFailure(err))
		return
	}
	c.logf("workbuddy: account %s used a makeup card on %s to keep the run alive", core.MaskSecret(a.ID()), yesterday)
}

// compactPrizePayload trims a prize payload to something a log line can hold.
func compactPrizePayload(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "-"
	}
	if len(s) > 220 {
		return s[:220] + "…"
	}
	return s
}
