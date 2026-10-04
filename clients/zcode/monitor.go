package zcode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"client2api/internal/core"
)

// Monitor quota is the other half of a ZCode account's entitlement picture.
//
// The plan-billing endpoint reports time-boxed token grants (a promotion, an
// API-key plan, ...).  The monitor endpoint reports the rolling windows the
// desktop client draws as "5 hours" and "today".  z-Switch probes both and
// merges them; reading only billing is why a perfectly healthy account can
// show no 5-hour quota here.
const (
	monitorProviderName = "open.bigmodel.cn/api/monitor"
)

// monitorEnvelope is the vendor's common business envelope.
type monitorEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type monitorQuotaDoc struct {
	Level  string         `json:"level"`
	Limits []monitorLimit `json:"limits"`
}

type monitorLimit struct {
	Type          string  `json:"type"`
	Unit          int64   `json:"unit"`
	Number        int64   `json:"number"`
	Usage         float64 `json:"usage"`
	CurrentValue  float64 `json:"currentValue"`
	Remaining     float64 `json:"remaining"`
	Percentage    float64 `json:"percentage"`
	NextResetTime int64   `json:"nextResetTime"`
}

type subscriptionDoc struct {
	ProductID       string `json:"productId"`
	ProductName     string `json:"productName"`
	Status          string `json:"status"`
	InCurrentPeriod bool   `json:"inCurrentPeriod"`
	ExpiresAt       int64  `json:"expires_at"`
}

// monitorTokenFor returns the credential that can serve the monitor endpoint.
// It is the coding-plan API key, not the plan JWT: the JWT is what the billing
// endpoints want, and sending it to open.bigmodel.cn answers 401.
func (c *Client) monitorTokenFor(acct *Account) string {
	if acct == nil {
		return ""
	}
	if acct.Mode == modeAPIKey {
		return strings.TrimSpace(acct.apiKey)
	}
	// A JWT channel can still have an API-key sibling.  The caller picks the
	// sibling before asking; this fallback keeps a manually-configured account
	// usable when the API key was placed in the jwt field by mistake.
	return ""
}

// monitorQuotaOf reads one account's rolling-window limits.
func (c *Client) monitorQuotaOf(ctx context.Context, acct *Account) (*monitorQuotaDoc, error) {
	token := c.monitorTokenFor(acct)
	if token == "" {
		return nil, fmt.Errorf("%w: monitor quota needs a bigmodel coding-plan API key; %s is %s",
			core.ErrNotConfigured, acct.ID, firstNonEmpty(acct.Mode, "unknown"))
	}

	var env monitorEnvelope
	if err := c.monitorDo(ctx, http.MethodGet, monitorQuotaPath, token, &env); err != nil {
		return nil, err
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("zcode: monitor quota refused (%d): %s", env.Code, strings.TrimSpace(env.Msg))
	}
	var doc monitorQuotaDoc
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &doc); err != nil {
			return nil, fmt.Errorf("zcode: decode %s: %w", monitorQuotaPath, err)
		}
	}
	return &doc, nil
}

// subscriptionOf reads the subscription list, which is only used to label the
// monitor rows with the plan tier and its expiry.  A failure is not fatal: the
// limits themselves are still useful without a product name.
func (c *Client) subscriptionOf(ctx context.Context, token string) []subscriptionDoc {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	var env monitorEnvelope
	if err := c.monitorDo(ctx, http.MethodGet, subscriptionPath, token, &env); err != nil || env.Code != 0 {
		return nil
	}
	var rows []subscriptionDoc
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &rows)
	}
	return rows
}

// monitorDo performs one authenticated open.bigmodel.cn monitor call.
func (c *Client) monitorDo(ctx context.Context, method, path, token string, out any) error {
	u := "https://open.bigmodel.cn" + path
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return fmt.Errorf("zcode: build %s request: %w", path, err)
	}
	setHeader(req.Header, "Authorization", "Bearer "+token)
	setHeader(req.Header, "User-Agent", c.pool.userAgent())
	setHeader(req.Header, "x-request-id", randomUUID())

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("zcode: %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body, claimMaxBytes)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("zcode: %s answered HTTP %d: %s", path, resp.StatusCode, clip(string(raw), 200))
	}
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("zcode: decode %s: %w", path, err)
	}
	return nil
}

// monitorPackages converts the monitor document into the panel's package rows.
// The rows use synthetic codes (monitor:TIME_LIMIT, monitor:TOKENS_LIMIT) so a
// caller can tell them apart from plan-billing entitlements.
func monitorPackages(doc *monitorQuotaDoc, subs []subscriptionDoc) []core.CreditPackage {
	if doc == nil {
		return nil
	}
	tier, expire := subscriptionLabel(subs)
	out := make([]core.CreditPackage, 0, len(doc.Limits))
	for _, l := range doc.Limits {
		if l.Type == "" {
			continue
		}
		name, unit := monitorRowName(l)
		pkg := core.CreditPackage{
			Name:           name,
			Remain:         int64(l.Remaining),
			Used:           int64(l.CurrentValue),
			Size:           int64(l.Usage),
			PackageCode:    "monitor",
			SubProductCode: "monitor:" + l.Type,
			SubProductName: unit,
			Cycle:          true,
		}
		if tier != "" {
			pkg.Name = tier + " · " + pkg.Name
		}
		if expire != "" {
			pkg.EndTime = expire
		} else if l.NextResetTime > 0 {
			pkg.ExpiresAt = l.NextResetTime / 1000
			pkg.EndTime = time.Unix(pkg.ExpiresAt, 0).UTC().Format(time.RFC3339)
		}
		out = append(out, pkg)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Remain > out[j].Remain })
	return out
}

// monitorRowName maps the vendor's type/unit pair to a human label.  The
// reference's unit mapping is the source of the "5 小时" wording.
func monitorRowName(l monitorLimit) (string, string) {
	period := monitorPeriod(l.Unit, l.Number)
	switch l.Type {
	case "TIME_LIMIT":
		return "使用时长（" + period + "）", "分钟"
	case "TOKENS_LIMIT":
		return "提示次数（" + period + "）", "次"
	default:
		return l.Type + "（" + period + "）", ""
	}
}

func monitorPeriod(unit, number int64) string {
	switch unit {
	case 3:
		if number <= 0 {
			number = 5
		}
		return fmt.Sprintf("每 %d 小时", number)
	case 4:
		return "每天"
	case 5:
		return "每月"
	case 6:
		return "每周"
	default:
		return "每周期"
	}
}

func subscriptionLabel(subs []subscriptionDoc) (tier, expire string) {
	for _, s := range subs {
		if !s.InCurrentPeriod || !strings.EqualFold(strings.TrimSpace(s.Status), "VALID") {
			continue
		}
		tier = strings.TrimSpace(s.ProductName)
		if s.ExpiresAt > 0 {
			expire = time.Unix(s.ExpiresAt, 0).UTC().Format(time.RFC3339)
		}
		return tier, expire
	}
	return "", ""
}

// monitorBalance folds the monitor rows into core.Balance.  The primary number
// is the first TIME_LIMIT row (the 5-hour window), falling back to the first
// row with a positive total.  This is the same preference z-Switch uses when
// it picks the "main" slot for its headline number.
func monitorBalance(doc *monitorQuotaDoc) (core.Balance, bool) {
	if doc == nil || len(doc.Limits) == 0 {
		return core.Balance{}, false
	}
	var main *monitorLimit
	for i := range doc.Limits {
		l := &doc.Limits[i]
		if l.Type == "TIME_LIMIT" && l.Usage > 0 {
			main = l
			break
		}
	}
	if main == nil {
		for i := range doc.Limits {
			l := &doc.Limits[i]
			if l.Usage > 0 {
				main = l
				break
			}
		}
	}
	if main == nil {
		return core.Balance{}, false
	}
	bal := core.Balance{
		Credits: int64(main.Remaining),
		Used:    main.CurrentValue,
		Total:   int64(main.Usage),
		Unit:    balanceUnit,
	}
	if main.NextResetTime > 0 {
		bal.EarliestAt = time.Unix(main.NextResetTime/1000, 0)
		bal.EarliestRemaining = int64(main.Remaining)
	}
	return bal, true
}

// siblingAccountsFor returns the enabled credentials that belong to the same
// vendor account as acct.  A plan JWT and a coding-plan API key are two
// channels of one Zhipu account, not two accounts; the panel groups them by
// Identity and the quota readers must do the same.
func (c *Client) siblingAccountsFor(acct *Account) []*Account {
	if c == nil || c.pool == nil || acct == nil {
		return nil
	}
	seen := map[string]bool{acct.ID: true}
	out := []*Account{acct}
	if strings.TrimSpace(acct.UserID) == "" {
		return out
	}
	c.pool.mu.Lock()
	defer c.pool.mu.Unlock()
	c.pool.ensureLocked()
	for _, a := range c.pool.accounts {
		if a == nil || !a.Enabled || seen[a.ID] || a.UserID != acct.UserID {
			continue
		}
		seen[a.ID] = true
		cp := *a
		out = append(out, &cp)
	}
	return out
}

// monitorChannelFor picks the sibling that can actually answer the monitor
// endpoint.  It prefers an API-key channel, because that is what the vendor
// documents for open.bigmodel.cn.
func (c *Client) monitorChannelFor(acct *Account) *Account {
	for _, a := range c.siblingAccountsFor(acct) {
		if a.Mode == modeAPIKey && strings.TrimSpace(a.apiKey) != "" {
			return a
		}
	}
	return nil
}

// mergePackageReports combines plan-billing and monitor rows, de-duplicating
// rows that describe the same entitlement.  The caller keeps the union rather
// than choosing one source: the reference does exactly that, and the two
// sources answer different questions.
func mergePackageReports(a, b core.PackageReport) core.PackageReport {
	out := core.PackageReport{Packages: make([]core.CreditPackage, 0, len(a.Packages)+len(b.Packages))}
	seen := map[string]bool{}
	add := func(p core.CreditPackage) {
		key := p.PackageCode + "|" + p.SubProductCode + "|" + p.Name
		if seen[key] {
			return
		}
		seen[key] = true
		out.Packages = append(out.Packages, p)
	}
	for _, p := range a.Packages {
		add(p)
	}
	for _, p := range b.Packages {
		add(p)
	}
	for _, p := range out.Packages {
		out.Remain += p.Remain
		out.Size += p.Size
	}
	sort.SliceStable(out.Packages, func(i, j int) bool { return out.Packages[i].Remain > out.Packages[j].Remain })
	return out
}

// planBalanceWithMonitor is the shared reader behind AccountBalance and
// AccountPackages.  It asks the billing channel and, when a sibling API key is
// available, the monitor channel, then merges both answers.
func (c *Client) planBalanceWithMonitor(ctx context.Context, acct *Account) (*planBalances, *monitorQuotaDoc, error) {
	var billing *planBalances
	if acct.Mode == modeJWT {
		doc, err := c.planBalanceOf(ctx, acct)
		if err != nil {
			return nil, nil, err
		}
		billing = doc
	}
	var monitor *monitorQuotaDoc
	if channel := c.monitorChannelFor(acct); channel != nil {
		doc, err := c.monitorQuotaOf(ctx, channel)
		if err == nil {
			monitor = doc
		} else if billing == nil {
			return nil, nil, err
		}
	}
	if billing == nil && monitor == nil {
		return nil, nil, fmt.Errorf("%w: no billing or monitor channel is available for %s", core.ErrNotConfigured, acct.ID)
	}
	return billing, monitor, nil
}

// monitorURLValues is a tiny helper so the monitor request's query parameters
// stay in one place if the vendor adds one.
func monitorURLValues() url.Values { return url.Values{} }
