package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// Credit / package reads, ported from the reference internal/upstream/client.go
// (MIT): CreditPackages (client.go:1715), UserResource (:1814),
// UserResourceDetailed (:1847), UserResourceDetailedWithExpiry (:1855),
// packageRemainUsed (:1939) and parsePackageEndTime (:1824).
//
// Why this port exists: the whole point of the billing-meter endpoints is that
// two accounts with an identical chore history can still differ by thousands of
// credits, and the difference lives in the *packages* (face value and origin),
// not in the aggregate.  The panel therefore needs the per-package breakdown and
// the expiry, and this module previously had only the endpoint constants.
//
// The response nesting is the trap the reference calls out explicitly: the
// gateway envelope is already unwrapped by doJSON / billingMeterJSON, so parsing
// starts at `Response` — wrapping it in another code/data layer yields zero
// accounts for every account (i.e. "no packages anywhere").

// packageEndLayout is the wall-clock format the upstream uses for package
// expiry, in the same fixed UTC+8 zone the rate-limit reset uses.
const packageEndLayout = "2006-01-02 15:04:05"

// CreditPackage is one credit package's breakdown.
type CreditPackage struct {
	Name   string `json:"name"`
	Remain int64  `json:"remain"`
	Used   int64  `json:"used"`
	Size   int64  `json:"size"`
	// EndTime is the package's cycle end (first of ExpiredTime /
	// PackageEndTime / CycleEndTime that carries a value).
	EndTime string `json:"end_time,omitempty"`
	// ExpiresAt is EndTime as a Unix-millisecond stamp, for day-accurate
	// aggregation in the panel.
	ExpiresAt int64 `json:"expires_at,omitempty"`
	// CreatedAt is when the package was granted (RFC3339).  It is the only way
	// to tell a "first authorisation gift" from a "campaign reward": the two
	// share the same PackageName and PackageCode, and only the timestamp says
	// which is which.
	CreatedAt string `json:"created_at,omitempty"`
	// PackageCode / SubProductCode identify the package type upstream.  A
	// different code under the same name is a different origin; the same code
	// with a different face value is the same origin granted in tranches.
	PackageCode    string `json:"package_code,omitempty"`
	SubProductCode string `json:"sub_product_code,omitempty"`
	SubProductName string `json:"sub_product_name,omitempty"`
	// Cycle marks a package measured by the Cycle* fields rather than Capacity*.
	Cycle bool `json:"cycle,omitempty"`
}

// respAccount is the package field set packageRemainUsed aggregates.
type respAccount struct {
	CapacityRemain      int64
	CapacityUsed        int64
	CapacitySize        int64
	CycleCapacityRemain int64
	CycleCapacityUsed   int64
	CycleCapacitySize   int64
}

// creditPackagesBody is the query both reads send.  It deliberately asks for a
// very wide end window: the upstream rejects a window that does not cover the
// package's own end time, so a narrow one silently hides long-lived packs.
func creditPackagesBody(now time.Time) map[string]any {
	return map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}
}

// CreditPackages returns the account's per-package breakdown plus the summed
// remain/size.  It is sorted by face value, descending, because that is where
// the account-to-account difference shows up first.
func (c *Client) CreditPackages(ctx context.Context, a *Auth) ([]CreditPackage, int64, int64, error) {
	now := time.Now()
	data, err := c.billingMeterJSON(ctx, a, http.MethodPost, creditPackagesBody(now))
	if err != nil {
		return nil, 0, 0, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CapacitySize        int64  `json:"CapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					ExpiredTime         string `json:"ExpiredTime"`
					PackageEndTime      string `json:"PackageEndTime"`
					CycleEndTime        string `json:"CycleEndTime"`
					CreateTime          int64  `json:"CreateTime"`
					PackageCode         string `json:"PackageCode"`
					SubProductCode      string `json:"SubProductCode"`
					SubProductName      string `json:"SubProductName"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, 0, 0, fmt.Errorf("packages parse: %w", err)
	}
	packs := resp.Response.Data.Accounts
	out := make([]CreditPackage, 0, len(packs))
	var sumRemain, sumSize int64
	for _, p := range packs {
		cp := CreditPackage{
			Name:           p.PackageName,
			PackageCode:    p.PackageCode,
			SubProductCode: p.SubProductCode,
			SubProductName: p.SubProductName,
		}
		switch {
		case p.ExpiredTime != "":
			cp.EndTime = p.ExpiredTime
		case p.PackageEndTime != "":
			cp.EndTime = p.PackageEndTime
		default:
			cp.EndTime = p.CycleEndTime
		}
		if cp.EndTime != "" {
			if end, perr := time.ParseInLocation(packageEndLayout, cp.EndTime, softRateResetLoc); perr == nil {
				cp.ExpiresAt = end.UnixMilli()
			}
		}
		// CreateTime is epoch milliseconds; 0 means "not reported", and is left
		// empty rather than faked as 1970.
		if p.CreateTime > 0 {
			cp.CreatedAt = time.UnixMilli(p.CreateTime).Format(time.RFC3339)
		}
		if p.CycleCapacitySize > 0 {
			cp.Cycle = true
			cp.Remain, cp.Size = p.CycleCapacityRemain, p.CycleCapacitySize
			cp.Used = cp.Size - cp.Remain
			if p.CycleCapacityUsed > cp.Used {
				cp.Used = p.CycleCapacityUsed
				cp.Remain = cp.Size - cp.Used
			}
			if cp.Remain < 0 {
				cp.Remain = 0
			}
		} else {
			cp.Remain, cp.Used, cp.Size = p.CapacityRemain, p.CapacityUsed, p.CapacitySize
			if cp.Used == 0 && cp.Size > cp.Remain {
				cp.Used = cp.Size - cp.Remain
			}
		}
		sumRemain += cp.Remain
		sumSize += cp.Size
		out = append(out, cp)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Size > out[j].Size })
	return out, sumRemain, sumSize, nil
}

// UserResource returns the account's remaining and total credits.
func (c *Client) UserResource(ctx context.Context, a *Auth) (remain, total int64, err error) {
	remain, total, _, err = c.UserResourceDetailed(ctx, a, 0)
	return remain, total, err
}

// parsePackageEndTime parses an upstream package end time.  An empty or
// malformed value reports false, and the caller then conservatively leaves that
// package out of the earliest-expiry routing.
func parsePackageEndTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(packageEndLayout, raw, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// UserResourceDetailed additionally reports the subset of credits that expire
// within soon: with soon > 0, a package whose CycleEndTime parses and lands at
// or before now+soon contributes to expiring, so the pool can spend those
// credits first instead of letting campaign credit lapse.  soon <= 0 disables
// the bucket.  expiring is always a subset of remain.
func (c *Client) UserResourceDetailed(ctx context.Context, a *Auth, soon time.Duration) (remain, total, expiring int64, err error) {
	remain, total, expiring, _, _, err = c.UserResourceDetailedWithExpiry(ctx, a, soon)
	return remain, total, expiring, err
}

// UserResourceDetailedWithExpiry additionally reports the earliest future expiry
// batch: earliestAt is the earliest usable expiry instant and
// earliestRemaining is the total remaining credit that expires at exactly that
// instant.  Already-expired packages, packages with no remaining credit, and
// packages whose end time is missing or unparseable never form a batch; with no
// usable batch both are zero.
func (c *Client) UserResourceDetailedWithExpiry(ctx context.Context, a *Auth, soon time.Duration) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64, err error) {
	now := time.Now()
	data, err := c.billingMeterJSON(ctx, a, http.MethodPost, creditPackagesBody(now))
	if err != nil {
		return 0, 0, 0, time.Time{}, 0, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CycleEndTime        string `json:"CycleEndTime"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, 0, time.Time{}, 0, fmt.Errorf("resource parse: %w", err)
	}
	for _, acct := range resp.Response.Data.Accounts {
		r, _, size := packageRemainUsed(respAccount{
			CapacityRemain:      acct.CapacityRemain,
			CapacityUsed:        acct.CapacityUsed,
			CapacitySize:        acct.CapacitySize,
			CycleCapacityRemain: acct.CycleCapacityRemain,
			CycleCapacityUsed:   acct.CycleCapacityUsed,
			CycleCapacitySize:   acct.CycleCapacitySize,
		})
		if r < 0 {
			r = 0
		}
		if size < r {
			size = r
		}
		remain += r
		total += size
		if r <= 0 {
			continue
		}
		end, ok := parsePackageEndTime(acct.CycleEndTime)
		if !ok || !end.After(now) {
			continue
		}
		if earliestAt.IsZero() || end.Before(earliestAt) {
			earliestAt = end
			earliestRemaining = r
		} else if end.Equal(earliestAt) {
			earliestRemaining += r
		}
		if soon > 0 && !end.After(now.Add(soon)) {
			expiring += r
		}
	}
	return remain, total, expiring, earliestAt, earliestRemaining, nil
}

// packageRemainUsed aggregates one package's remain/used/size.  The cycle fields
// win when present; used is the larger of CycleUsed and size-remain, and remain
// is clamped into [0, size] so dirty upstream data cannot overstate a balance.
func packageRemainUsed(a respAccount) (remain, used, size int64) {
	if a.CycleCapacitySize > 0 {
		remain = a.CycleCapacityRemain
		size = a.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		if a.CycleCapacityUsed > used {
			used = a.CycleCapacityUsed
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	remain = a.CapacityRemain
	used = a.CapacityUsed
	size = a.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}
