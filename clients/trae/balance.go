package trae

// balance.go implements the panel's BalanceProvider for Trae CN.
//
// The upstream entitlement endpoint is the only balance read the reference
// implementation uses:
//
//	POST /trae/api/v2/pay/ide_user_ent_usage
//
// It returns user_entitlement_pack_list[] plus usage_summary.  The official
// Trae clients render usage_summary as the aggregate position and use the
// pack list for the per-grant breakdown.  The live capture on 2026-10-04
// shows why the enable_solo_* flags cannot be used as an eligibility filter:
// a usable welfare grant has credits_limit=4000 without those flags, while a
// fully spent monthly bonus carries all of them.  Filtering on the flags
// therefore reports 0/500 while the vendor still serves requests against the
// hidden 4000-credit grant.
//
// The endpoint speaks the light CN "ug" identity, not the SOLO chat fingerprint
// set: api.trae.cn, Cloud-IDE-JWT, X-User-Region and the device id.

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"client2api/internal/core"
)

const (
	// ugEntUsagePath is the aggregate entitlement/credit endpoint.
	ugEntUsagePath = "/trae/api/v2/pay/ide_user_ent_usage"

	// traeBalanceUnit is what this vendor's credits are called in the panel.
	traeBalanceUnit = "积分"
)

// ugEntQuota is one entitlement pack's quota.  The enable_solo_* flags are the
// vendor's own statement about which channel the pack may serve.
type ugEntQuota struct {
	CreditsLimit      int64 `json:"credits_limit"`
	EnableSoloLite    bool  `json:"enable_solo_lite"`
	EnableSoloCoder   bool  `json:"enable_solo_coder"`
	EnableSoloAgent   bool  `json:"enable_solo_agent"`
	EnableSoloBuilder bool  `json:"enable_solo_builder"`
	EnableSoloWeb     bool  `json:"enable_solo_web"`
}

// soloCapable reports whether this pack's quota may serve the SOLO channel this
// module reverse proxies.
func (q ugEntQuota) soloCapable() bool {
	return q.EnableSoloLite || q.EnableSoloCoder || q.EnableSoloAgent || q.EnableSoloBuilder || q.EnableSoloWeb
}

// ugEntUsageSummary is the vendor's aggregate view.  Pointers distinguish an
// absent summary from a literal zero.
type ugEntUsageSummary struct {
	TotalAmount    *float64 `json:"total_amount"`
	ConsumedAmount *float64 `json:"consumed_amount"`
}

// ugEntUsageReply is the subset of the entitlement response this module needs.
type ugEntUsageReply struct {
	UsageSummary            ugEntUsageSummary `json:"usage_summary"`
	UserEntitlementPackList []struct {
		EntitlementBaseInfo struct {
			Quota ugEntQuota `json:"quota"`
		} `json:"entitlement_base_info"`
		Usage struct {
			CreditsAmount float64 `json:"credits_amount"`
		} `json:"usage"`
	} `json:"user_entitlement_pack_list"`
}

// AccountBalance implements core.BalanceProvider.
//
// A vendor error is returned as an error: the panel has no honest number to
// display when the upstream refused the read.  A successful response with a
// missing summary falls back to the packs, and a genuinely zero balance is a
// valid answer rather than an error.
func (c *Client) AccountBalance(ctx context.Context, id string, soon time.Duration) (core.Balance, error) {
	if c == nil {
		return core.Balance{}, fmt.Errorf("trae: no client")
	}
	key := strings.TrimSpace(id)
	if key == "" {
		return core.Balance{}, fmt.Errorf("trae: no account id was given")
	}

	a := c.findAuth(key)
	if a == nil {
		if c.parkedAccount(key) {
			return core.Balance{}, fmt.Errorf("trae: account %q is disabled", key)
		}
		return core.Balance{}, fmt.Errorf("trae: account %q not found", key)
	}
	if !checkinEligible(a) {
		return core.Balance{}, fmt.Errorf(
			"trae: balance is only available to CN accounts; this one is %q",
			firstNonEmpty(a.Region, a.Product, "unknown"),
		)
	}

	rctx, cancel := context.WithTimeout(ctx, checkinTimeout)
	defer cancel()

	var reply ugEntUsageReply
	if err := c.doJSON(rctx, c.cfg.authHost()+ugEntUsagePath, c.ugHeaders(a), []byte("{}"), &reply); err != nil {
		return core.Balance{}, err
	}

	var (
		total int64
		used  float64
	)
	unlimited := false
	if reply.UsageSummary.TotalAmount != nil {
		totalAmount := *reply.UsageSummary.TotalAmount
		if totalAmount < 0 {
			unlimited = true
		} else {
			total = int64(math.Round(totalAmount))
		}
		if reply.UsageSummary.ConsumedAmount != nil {
			used = *reply.UsageSummary.ConsumedAmount
		}
	} else {
		for _, pack := range reply.UserEntitlementPackList {
			limit := pack.EntitlementBaseInfo.Quota.CreditsLimit
			switch {
			case limit < 0:
				unlimited = true
			case limit > 0:
				total += limit
			}
			used += pack.Usage.CreditsAmount
		}
	}
	if used < 0 {
		used = 0
	}
	if unlimited {
		return core.Balance{
			Used:      used,
			Unlimited: true,
			Unit:      traeBalanceUnit,
		}, nil
	}

	// Round consumption up when the vendor reports a fraction: the panel's
	// remaining credit must never overstate what is actually left.
	remaining := total - int64(math.Ceil(used))
	if remaining < 0 {
		remaining = 0
	}
	return core.Balance{
		Credits: remaining,
		Used:    used,
		Total:   total,
		Unit:    traeBalanceUnit,
	}, nil
}

// Compile-time proof that this module satisfies the optional balance contract.
var _ core.BalanceProvider = (*Client)(nil)
