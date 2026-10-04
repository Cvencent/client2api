package workbuddy

import (
	"context"
	"encoding/json"
	"net/http"
)

// Growth-centre streak redemption + lottery.
//
// Ported from the reference internal/upstream/streak.go (MIT).  Mechanism: the
// login tiers (7d/14d/28d) unlock by *consecutive* login days; redeeming posts
// to /activity/growth/redeem and pays credit / energy / makeup cards / lottery
// chances; drawing spends one chance.  An unlocked-by-days refusal comes back as
// HTTP 403, which the caller is expected to treat as "skip me today", not as a
// transport failure.
//
// These are CN growth-domain routes: the reference does not realm-branch them,
// and neither does this port (the same chat host path family answers for both
// realms; a realm that has no such activity simply returns a business code).
//
// The streak *read* lives in travel.go (streakPath), matching the reference
// layout, because the board and the buddy panel read the same streak object.
// The idempotency token comes from the existing clientToken() in tasks.go —
// the reference has an identical function in streak.go:27-33 and this port
// reuses ours instead of defining a second one.

const (
	streakRedeemPath   = "/activity/growth/redeem"
	lotterySummaryPath = "/activity/growth/lottery/summary"
	lotteryDrawPath    = "/activity/growth/lottery/draw"
)

// StreakDays is the consecutive-login block of StreakFull.
type StreakDays struct {
	Days              int    `json:"days"`
	MonthTotalDays    int    `json:"month_total_days"`
	NextTier          string `json:"next_tier"`
	NextTierRemaining int    `json:"next_tier_remaining"`
}

// StreakMakeupCards is the makeup-card wallet (balance / cap).
type StreakMakeupCards struct {
	Balance int `json:"balance"`
	Max     int `json:"max"`
}

// StreakTier is one redeemable tier of the redemption table.
type StreakTier struct {
	Tier    string `json:"tier"`
	Days    int    `json:"days"`
	Credit  int    `json:"credit"`
	Energy  int    `json:"energy"`
	Cards   int    `json:"cards"`
	Chances int    `json:"chances"`
}

// StreakRedemption is the redemption status block.
type StreakRedemption struct {
	Tier7dStatus  string       `json:"tier_7d_status"`
	Tier14dStatus string       `json:"tier_14d_status"`
	Tier28dStatus string       `json:"tier_28d_status"`
	RemainingDays int          `json:"remaining_days"`
	Tiers         []StreakTier `json:"tiers"`
}

// StreakFull is the whole GET /activity/growth/streak payload.
type StreakFull struct {
	Streak           StreakDays        `json:"streak"`
	MakeupCards      StreakMakeupCards `json:"makeup_cards"`
	RedemptionStatus StreakRedemption  `json:"redemption_status"`
}

// GrowthStreakFull reads the full consecutive-login state.
func (c *Client) GrowthStreakFull(ctx context.Context, a *Auth) (*StreakFull, error) {
	data, err := c.growthCall(ctx, a, http.MethodGet, streakPath, nil, false)
	if err != nil {
		return nil, err
	}
	out := &StreakFull{}
	if err := decodePayload(data, out); err != nil {
		return nil, err
	}
	return out, nil
}

// GrowthRedeemTier redeems one login tier ("7d" | "14d" | "28d").  A tier that is
// not unlocked yet is an *Error (HTTP 403): the caller skips it and retries
// another day, exactly as the reference does.
func (c *Client) GrowthRedeemTier(ctx context.Context, a *Auth, tier string) error {
	_, err := c.growthCall(ctx, a, http.MethodPost, streakRedeemPath, map[string]any{
		"tier":         tier,
		"client_token": clientToken(),
	}, false)
	return err
}

// LotteryChances reports how many draws are available.
func (c *Client) LotteryChances(ctx context.Context, a *Auth) (int, error) {
	data, err := c.growthCall(ctx, a, http.MethodGet, lotterySummaryPath, nil, false)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Chances int `json:"chances"`
		Module  struct {
			Enabled bool `json:"enabled"`
		} `json:"module"`
	}
	if err := decodePayload(data, &resp); err != nil {
		return 0, err
	}
	return resp.Chances, nil
}

// LotteryDraw spends one chance and returns the raw prize payload: its shape is
// decided by whichever campaign is running, so it is passed through untouched.
func (c *Client) LotteryDraw(ctx context.Context, a *Auth) (json.RawMessage, error) {
	return c.growthCall(ctx, a, http.MethodPost, lotteryDrawPath, map[string]any{
		"client_token": clientToken(),
	}, false)
}
