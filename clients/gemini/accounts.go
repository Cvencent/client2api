package gemini

import (
	"context"
	"fmt"
	"strings"
	"time"

	"client2api/internal/core"
)

// accounts.go implements the panel's account management surface:
// core.AccountManager, core.CredentialImporter, core.LoginProvider and
// core.BalanceProvider.

var (
	_ core.AccountManager     = (*Client)(nil)
	_ core.CredentialImporter = (*Client)(nil)
	_ core.BalanceProvider    = (*Client)(nil)
)

// AccountFields describes the "add account" form.
//
// A Google account is normally minted by the sign-in button, so the form exists
// for the paste path: an access/refresh token pair copied from another machine.
func (c *Client) AccountFields(ctx context.Context) []core.FieldSpec {
	return []core.FieldSpec{
		{Key: "refresh_token", Label: "Refresh token", Type: "password", Help: "可留空；不填则需要填写 access_token"},
		{Key: "access_token", Label: "Access token", Type: "password", Advanced: true, Help: "临时令牌，expiry 到期后需要 refresh_token 续期"},
		{Key: "expiry", Label: "过期时间", Type: "text", Advanced: true, Placeholder: "RFC3339，例如 2026-10-14T12:00:00Z"},
		{Key: "email", Label: "邮箱", Type: "text", Advanced: true, Help: "仅用于显示"},
		{Key: "sub", Label: "账号 ID", Type: "text", Advanced: true, Help: "Google 的稳定用户标识，用于区分账号"},
		{Key: "cloudaicompanionProject", Label: "项目号", Type: "text", Advanced: true, Help: "留空自动探测"},
		{Key: "label", Label: "备注", Type: "text", Advanced: true},
	}
}

// Accounts lists the credentials this module holds.
func (c *Client) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	c.ensure()
	now := c.now()
	out := make([]core.AccountRecord, 0, c.pool.len())
	for _, a := range c.pool.snapshot() {
		rec := core.AccountRecord{
			ID:        a.ID,
			Label:     a.Label,
			Enabled:   a.Enabled,
			State:     stateOf(&a, now),
			ExpiresAt: a.Expiry,
			Note:      noteOf(&a, now),
			Identity:  a.Identity,
		}
		fields := map[string]any{}
		if a.Email != "" {
			fields["email"] = a.Email
		}
		if a.Sub != "" {
			fields["sub"] = a.Sub
		}
		if a.Project != "" {
			fields["project"] = a.Project
		}
		if a.Tier != "" {
			fields["tier"] = a.Tier
		}
		if len(fields) > 0 {
			rec.Fields = fields
		}
		out = append(out, rec)
	}
	return out, nil
}

// AddAccount stores a pasted credential.
func (c *Client) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	c.ensure()
	access := strings.TrimSpace(spec.Fields["access_token"])
	refresh := strings.TrimSpace(spec.Fields["refresh_token"])
	if access == "" && refresh == "" {
		return core.AccountRecord{}, fmt.Errorf("refresh_token or access_token is required")
	}
	cfg := AccountConfig{
		Label:              strings.TrimSpace(spec.Fields["label"]),
		AccessToken:        access,
		RefreshToken:       refresh,
		Expiry:             strings.TrimSpace(spec.Fields["expiry"]),
		Email:              strings.TrimSpace(spec.Fields["email"]),
		Sub:                strings.TrimSpace(spec.Fields["sub"]),
		CloudAICodeProject: strings.TrimSpace(spec.Fields["cloudaicompanionProject"]),
	}
	if cfg.Expiry == "" && access != "" {
		cfg.Expiry = expiryFrom(3600)
	}
	rec := accountFromConfig(cfg, c.now())
	if spec.ID != "" {
		rec.ID = spec.ID
	}
	if spec.Label != "" {
		rec.Label = spec.Label
	}
	if err := c.upsertAccount(rec); err != nil {
		return core.AccountRecord{}, err
	}
	return core.AccountRecord{
		ID: rec.ID, Label: rec.Label, Enabled: rec.Enabled,
		State: "ready", Identity: rec.Identity, ExpiresAt: rec.Expiry,
	}, nil
}

// RemoveAccount deletes one credential.
func (c *Client) RemoveAccount(ctx context.Context, id string) error {
	c.ensure()
	if !c.pool.remove(id) {
		return fmt.Errorf("account %s not found", id)
	}
	c.persistPool()
	return nil
}

// SetAccountEnabled toggles one credential.
func (c *Client) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	c.ensure()
	if !c.pool.setEnabled(id, enabled) {
		return fmt.Errorf("account %s not found", id)
	}
	c.persistPool()
	return nil
}

// TestAccount probes one credential by asking the vendor for the account tier.
func (c *Client) TestAccount(ctx context.Context, id string) (core.TestResult, error) {
	c.ensure()
	acct := c.pool.byID(id)
	if acct == nil {
		return core.TestResult{AccountID: id, Error: "account not found"}, nil
	}
	start := time.Now()
	project, tier := c.probeProject(ctx, acct)
	res := core.TestResult{AccountID: id, ElapsedMS: time.Since(start).Milliseconds()}
	if project == "" && tier == "" {
		res.OK = false
		res.Error = "loadCodeAssist did not answer; the credential may be expired"
		return res, nil
	}
	res.OK = true
	res.Model = upstreamFlash
	res.Reply = fmt.Sprintf("project=%s tier=%s", firstNonEmpty(project, defaultProject), firstNonEmpty(tier, "unknown"))
	return res, nil
}

// RefreshAccount renews one credential (or every credential when id is empty).
func (c *Client) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	c.ensure()
	var targets []accountRecord
	if strings.TrimSpace(id) == "" {
		targets = c.pool.snapshot()
	} else {
		if a := c.pool.byID(id); a != nil {
			targets = []accountRecord{*a}
		}
	}
	results := make([]core.RefreshResult, 0, len(targets))
	for _, a := range targets {
		res := core.RefreshResult{AccountID: a.ID}
		if a.RefreshToken == "" {
			res.OK = false
			res.Error = "no refresh_token; sign in again"
			results = append(results, res)
			continue
		}
		grant, err := c.refreshGrant(ctx, a.RefreshToken)
		if err != nil {
			res.OK = false
			res.Error = core.Redact(err.Error())
			results = append(results, res)
			continue
		}
		expiry := expiryFrom(grant.ExpiresIn)
		c.pool.updateTokens(a.ID, grant.AccessToken, grant.RefreshToken, expiry, grant.TokenType)
		res.OK = true
		results = append(results, res)
	}
	c.persistPool()
	return results, nil
}

// --- credential import ------------------------------------------------------

// Discover reports Google OAuth token files found beside the Cloud Code tools
// this machine runs.
func (c *Client) Discover(ctx context.Context) ([]core.DiscoveredCredential, error) {
	var out []core.DiscoveredCredential
	for _, p := range discoverCredentialPaths() {
		out = append(out, core.DiscoveredCredential{
			Path:       p,
			Kind:       "google-oauth",
			Label:      "Google OAuth token",
			Importable: true,
		})
	}
	return out, nil
}

// Import imports the named paths (or everything Discover found).
func (c *Client) Import(ctx context.Context, paths []string, all bool) ([]core.AccountRecord, error) {
	c.ensure()
	if !all && len(paths) == 0 {
		return nil, nil
	}
	if all {
		for _, d := range discoverCredentialPaths() {
			paths = append(paths, d)
		}
	}
	var imported []core.AccountRecord
	for _, p := range paths {
		data, err := readFileLimited(p)
		if err != nil {
			continue
		}
		cfgs, err := decodeCredentialsBody(data)
		if err != nil {
			continue
		}
		for _, cfg := range cfgs {
			if cfg.AccessToken == "" && cfg.RefreshToken == "" {
				continue
			}
			rec := accountFromConfig(cfg, c.now())
			if rec.ID == "" {
				continue
			}
			if err := c.upsertAccount(rec); err != nil {
				continue
			}
			imported = append(imported, core.AccountRecord{ID: rec.ID, Label: rec.Label, Enabled: true, Identity: rec.Identity, State: "ready"})
		}
	}
	return imported, nil
}

// --- login ------------------------------------------------------------------

// StartLogin implements core.LoginProvider.  It starts the OAuth flow and
// returns the URL immediately.
func (c *Client) StartLogin(ctx context.Context) (core.LoginState, error) {
	c.ensure()
	session, err := c.startOAuthFlow()
	if err != nil {
		return core.LoginState{}, err
	}
	sessionID := randomHex(12)
	c.loginMu.Lock()
	c.logins[sessionID] = session
	c.loginMu.Unlock()
	return core.LoginState{SessionID: sessionID, State: "pending", URL: session.loginURL}, nil
}

// PollLogin implements core.LoginProvider.
func (c *Client) PollLogin(ctx context.Context, sessionID string) (core.LoginState, error) {
	c.ensure()
	c.loginMu.Lock()
	session := c.logins[sessionID]
	c.loginMu.Unlock()
	if session == nil {
		return core.LoginState{State: "failed", Message: "unknown login session"}, nil
	}
	state, accountID, message, finished := session.snapshot()
	st := core.LoginState{SessionID: sessionID, State: state, URL: session.loginURL, Message: message, AccountID: accountID}
	if state == "pending" {
		st.URL = session.loginURL
	}
	if finished {
		c.loginMu.Lock()
		delete(c.logins, sessionID)
		c.loginMu.Unlock()
		session.close()
	}
	return st, nil
}

// CancelLogin implements core.LoginProvider.
func (c *Client) CancelLogin(ctx context.Context, sessionID string) error {
	c.ensure()
	c.loginMu.Lock()
	session := c.logins[sessionID]
	delete(c.logins, sessionID)
	c.loginMu.Unlock()
	if session == nil {
		return nil
	}
	session.setResult("cancelled", "", "cancelled by the operator")
	session.close()
	return nil
}

// --- balance ----------------------------------------------------------------

// AccountBalance implements core.BalanceProvider.  Gemini's quota is a pair of
// rolling windows, not a credit number, so the balance is reported as a
// percentage and the two windows ride along as BalanceLine rows.
func (c *Client) AccountBalance(ctx context.Context, id string, soon time.Duration) (core.Balance, error) {
	c.ensure()
	acct := c.pool.byID(id)
	if acct == nil {
		return core.Balance{}, fmt.Errorf("account %s not found", id)
	}
	fiveHour, weekly, err := c.fetchQuotaWindows(ctx, acct)
	if err != nil {
		return core.Balance{}, err
	}
	if fiveHour == nil && weekly == nil {
		return core.Balance{}, fmt.Errorf("the quota response carried no Gemini bucket")
	}
	bal := core.Balance{Unit: "%"}
	if weekly != nil {
		bal.Credits = int64(percentOf(weekly))
		bal.Total = 100
	}
	if fiveHour != nil {
		bal.Lines = append(bal.Lines, quotaLine("5 小时窗口", fiveHour))
	}
	if weekly != nil {
		bal.Lines = append(bal.Lines, quotaLine("周窗口", weekly))
	}
	// A quota window is not a currency: expiring-credit routing does not
	// apply, so `soon` is deliberately ignored.
	_ = soon
	return bal, nil
}

func quotaLine(label string, w *quotaWindow) core.BalanceLine {
	remaining := float64(percentOf(w))
	used := 100 - remaining
	return core.BalanceLine{
		Label:            label,
		Unit:             "%",
		RemainingPercent: &remaining,
		UsedPercent:      &used,
		ResetsAt:         w.ResetTime,
	}
}

// percentOf clamps a remaining fraction into an integer percentage.
func percentOf(w *quotaWindow) int {
	if w == nil {
		return 0
	}
	f := w.RemainingFraction
	if f < 0 {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	return int(f*100 + 0.5)
}
