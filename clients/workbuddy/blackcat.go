package workbuddy

import (
	"context"
	"net/http"
	"time"
)

// Gift / compensation claiming, the heatmap read and makeup cards.
//
// Ported from the reference internal/upstream/blackcat.go (MIT), which owns the
// night-chat chore too.  The night-chat loop is NOT re-ported here: this module
// already implements black_cat (tasks.go runBlackCat, window inNightWindow), and
// a second owner for that chore would let two runners report the same events.

const (
	claimGiftPath         = "/billing/meter/claim-gift"
	claimCompensationPath = "/billing/meter/claim-compensation"
	heatmapPath           = "/activity/growth/heatmap"
	makeupCardUsePath     = "/activity/growth/makeup-cards/use"
)

// ClaimGift claims the one-per-account starter gift.  Already claimed is a
// business-code refusal, which comes back as an *Error.
func (c *Client) ClaimGift(ctx context.Context, a *Auth) (int64, error) {
	data, err := c.billingJSON(ctx, a, http.MethodPost, claimGiftPath, map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	if err := decodePayload(data, &resp); err != nil {
		return 0, err
	}
	return resp.Credit, nil
}

// ClaimCompensation claims an activity compensation when one is pending.
func (c *Client) ClaimCompensation(ctx context.Context, a *Auth) (int64, error) {
	data, err := c.billingJSON(ctx, a, http.MethodPost, claimCompensationPath, map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	if err := decodePayload(data, &resp); err != nil {
		return 0, err
	}
	return resp.Credit, nil
}

// HeatmapYesterdayMissed reports whether yesterday's check-in was missed, which
// is the heatmap cell for yesterday carrying score 0.  A day that is not in the
// heatmap at all is reported as "not missed" (there is nothing to mend).
func (c *Client) HeatmapYesterdayMissed(ctx context.Context, a *Auth) (bool, error) {
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	data, err := c.growthCall(ctx, a, http.MethodGet, heatmapPath, nil, false)
	if err != nil {
		return false, err
	}
	var resp struct {
		Cells []struct {
			Date  string `json:"date"`
			Score int    `json:"score"`
		} `json:"cells"`
	}
	if err := decodePayload(data, &resp); err != nil {
		return false, err
	}
	for _, cell := range resp.Cells {
		if len(cell.Date) >= 10 && cell.Date[:10] == yesterday {
			return cell.Score == 0, nil
		}
	}
	return false, nil
}

// UseMakeupCard spends a makeup card on targetDate ("2006-01-02") to keep the
// consecutive-login run alive.  No card is a business-code refusal.
func (c *Client) UseMakeupCard(ctx context.Context, a *Auth, targetDate string) error {
	_, err := c.growthCall(ctx, a, http.MethodPost, makeupCardUsePath, map[string]any{
		"target_date": targetDate,
	}, false)
	return err
}
