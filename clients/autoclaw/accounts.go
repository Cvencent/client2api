package autoclaw

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"client2api/internal/core"
)

var (
	_ core.AccountManager      = (*Client)(nil)
	_ core.CredentialImporter  = (*Client)(nil)
	_ core.Reviver             = (*Client)(nil)
	_ core.ModelRefresher      = (*Client)(nil)
	_ core.ModelLimitsProvider = (*Client)(nil)
	_ core.BalanceProvider     = (*Client)(nil)
	_ core.PackageProvider     = (*Client)(nil)
)

const probeTimeout = 30 * time.Second

func (c *Client) AccountFields(ctx context.Context) []core.FieldSpec {
	return []core.FieldSpec{
		{Key: "access_token", Label: "Access token", Type: "password", Required: true,
			Help: "AutoClaw 访问令牌。可从 AutoClaw 客户端账号目录的 account-credentials.enc 导入，通常带有 Bearer 前缀也可直接粘贴。"},
		{Key: "refresh_token", Label: "Refresh token", Type: "password",
			Help: "强烈建议填写；否则 access token 过期后账号无法自动续期。"},
		{Key: "account_id", Label: "Account ID", Type: "text", Advanced: true,
			Help: "AutoClaw 客户端 profile 中的 accountId，用于稳定标识账号。"},
		{Key: "device_id", Label: "Device ID", Type: "text", Advanced: true,
			Help: "AutoClaw 设备号，可通过本地导入自动获取。"},
		{Key: "user_id", Label: "User ID", Type: "text", Advanced: true},
		{Key: "numeric_user_id", Label: "Numeric user ID", Type: "text", Advanced: true},
		{Key: "phone", Label: "Phone", Type: "text"},
		{Key: "nickname", Label: "Nickname", Type: "text"},
		{Key: "label", Label: "Label", Type: "text"},
	}
}

func (c *Client) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	return c.pool.panelRecords(), nil
}

func (c *Client) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	tok := strings.TrimSpace(spec.Field("access_token"))
	if tok == "" {
		return core.AccountRecord{}, errors.New("autoclaw: access_token is required")
	}
	a := account{credential: credential{
		AccountID:     strings.TrimSpace(spec.Field("account_id")),
		AccessToken:   tok,
		RefreshToken:  strings.TrimSpace(spec.Field("refresh_token")),
		ExpiresAt:     flexString(strings.TrimSpace(spec.Field("expires_at"))),
		UserID:        strings.TrimSpace(spec.Field("user_id")),
		NumericUserID: strings.TrimSpace(spec.Field("numeric_user_id")),
		Nickname:      strings.TrimSpace(spec.Field("nickname")),
		Phone:         strings.TrimSpace(spec.Field("phone")),
		DeviceID:      strings.TrimSpace(spec.Field("device_id")),
	}, Label: strings.TrimSpace(spec.Label), Origin: originStored}
	if id := strings.TrimSpace(spec.ID); id != "" {
		a.ID = id
	}
	e := c.pool.put(a)
	if !spec.EnabledOr(true) {
		_ = c.pool.setEnabled(e.acct.id(), false)
	}
	return c.pool.record(e.acct.id()), nil
}

func (c *Client) RemoveAccount(ctx context.Context, id string) error {
	e := c.pool.find(id)
	if e == nil {
		return fmt.Errorf("autoclaw: unknown account %q", id)
	}
	if e.acct.Origin == originConfig || e.acct.Origin == originEnv {
		return fmt.Errorf("autoclaw: account %q comes from the module config; remove it there", id)
	}
	if !c.pool.remove(id) {
		return fmt.Errorf("autoclaw: unknown account %q", id)
	}
	return nil
}

func (c *Client) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	return c.pool.setEnabled(id, enabled)
}

func (c *Client) ReviveAccount(ctx context.Context, id string) error {
	e := c.pool.find(id)
	if e == nil {
		return fmt.Errorf("autoclaw: unknown account %q", id)
	}
	c.pool.revive(e)
	return nil
}

func (c *Client) TestAccountModel(ctx context.Context, id, wantModel string) (core.TestResult, error) {
	e := c.pool.find(id)
	if e == nil {
		return core.TestResult{}, fmt.Errorf("autoclaw: unknown account %q", id)
	}
	ctx, cancel := withTimeout(ctx, probeTimeout)
	defer cancel()
	start := time.Now()
	model := strings.TrimSpace(wantModel)
	if model == "" {
		model = c.probeModelID()
	}
	reply, err := c.probeChat(ctx, e, model)
	elapsed := time.Since(start).Milliseconds()
	res := core.TestResult{AccountID: id, Model: model, ElapsedMS: elapsed}
	if err != nil {
		res.Error = core.Redact(err.Error())
		c.pool.markFailure(e, classifyErr(err), err.Error())
		return res, nil
	}
	res.OK = true
	res.Reply = reply
	c.pool.markUsed(e)
	return res, nil
}

// TestAccount keeps the pre-model signature: it probes with the module's own
// default model, which is what every caller before the model picker meant.
func (c *Client) TestAccount(ctx context.Context, id string) (core.TestResult, error) {
	return c.TestAccountModel(ctx, id, "")
}

func (c *Client) probeModelID() string {
	ids := c.modelIDs()
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func (c *Client) probeChat(ctx context.Context, e *entry, model string) (string, error) {
	if strings.TrimSpace(model) == "" {
		return "", errors.New("autoclaw: no model is available to probe with")
	}
	maxTokens := 16
	req := &core.ChatRequest{Model: model, Messages: []core.Message{{Role: "user", Content: "ping"}}, MaxTokens: &maxTokens}
	stream, err := c.chatWith(ctx, nil, e, req, mapModel(model))
	if err != nil {
		return "", err
	}
	defer stream.Close()
	var reply strings.Builder
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if reply.Len() > 0 {
				return reply.String(), nil
			}
			return "", err
		}
		if event.Type == core.EventDelta {
			reply.WriteString(event.Delta)
		}
		if event.Type == core.EventError && event.Err != nil {
			if reply.Len() > 0 {
				return reply.String(), nil
			}
			return "", event.Err
		}
	}
	if reply.Len() == 0 {
		return "", fmt.Errorf("autoclaw: %s answered without any text", model)
	}
	return reply.String(), nil
}

func (c *Client) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	var targets []*entry
	if strings.TrimSpace(id) == "" {
		targets = c.pool.all()
	} else {
		e := c.pool.find(id)
		if e == nil {
			return nil, fmt.Errorf("autoclaw: unknown account %q", id)
		}
		targets = []*entry{e}
	}
	out := make([]core.RefreshResult, 0, len(targets))
	for _, e := range targets {
		res := core.RefreshResult{AccountID: e.acct.id()}
		if err := c.refreshAccount(ctx, e); err != nil {
			res.Error = core.Redact(err.Error())
		} else {
			res.OK = true
		}
		out = append(out, res)
	}
	return out, nil
}

func (c *Client) refreshAccount(ctx context.Context, e *entry) error {
	ctx, cancel := withTimeout(ctx, c.cfg.modelsTimeout())
	defer cancel()
	res, err := c.refresh(ctx, e.acct.cred())
	if err != nil {
		if errors.Is(err, errSessionDead) {
			c.pool.markDead(e, "refresh token rejected; sign in again")
		}
		return err
	}
	c.pool.applyRefresh(e.acct.id(), res)
	return nil
}

func (p *pool) panelRecords() []core.AccountRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := make([]core.AccountRecord, 0, len(p.entries))
	for _, e := range p.entries {
		p.usableLocked(e, now)
		fields := map[string]any{
			"origin": e.acct.Origin, "failures": e.fails,
			"refreshable": e.acct.cred().refreshable(),
			"removable":   e.acct.Origin == originStored || e.acct.Origin == originImport,
		}
		if e.acct.AccountID != "" {
			fields["account_id"] = e.acct.AccountID
		}
		if e.acct.UserID != "" {
			fields["user_id"] = e.acct.UserID
		}
		if e.acct.NumericUserID != "" {
			fields["numeric_user_id"] = e.acct.NumericUserID
		}
		if e.acct.Nickname != "" {
			fields["nickname"] = e.acct.Nickname
		}
		if e.acct.Phone != "" {
			fields["phone"] = maskPhone(e.acct.Phone)
		}
		if e.acct.DeviceID != "" {
			fields["device_id"] = e.acct.DeviceID
		}
		rec := core.AccountRecord{
			ID: e.acct.id(), Label: panelLabel(e.acct), Enabled: !e.acct.Disabled,
			State: e.state, Note: e.note, Fields: fields, Identity: e.acct.cred().identity(),
		}
		// expires_at is published at the TOP level, not just as expires_in:
		// the background renewal sweep (cmd/client2api/renew.go) reads
		// AccountRecord.ExpiresAt and skips any account whose field is empty,
		// so omitting it here silently excluded every AutoClaw account from
		// proactive renewal.
		if ms, ok := e.acct.cred().expiresAtMs(); ok {
			exp := time.UnixMilli(ms).UTC()
			rec.ExpiresAt = exp.Format(time.RFC3339)
			fields["expires_in"] = int64(exp.Sub(now).Seconds())
		}
		if !e.until.IsZero() && e.until.After(now) {
			fields["cooldown_until"] = e.until.UTC().Format(time.RFC3339)
		}
		out = append(out, rec)
	}
	return out
}

func (p *pool) record(id string) core.AccountRecord {
	for _, r := range p.panelRecords() {
		if r.ID == id {
			return r
		}
	}
	return core.AccountRecord{ID: id}
}

func classifyErr(err error) errKind {
	if err == nil {
		return kindNone
	}
	if errors.Is(err, errSessionDead) {
		return kindAuth
	}
	var ae *apiError
	if errors.As(err, &ae) {
		switch {
		case ae.Status == 401 || ae.Status == 403 || ae.Code == refreshCodeDead || ae.Code == 410000:
			return kindAuth
		case ae.Status == 402 || ae.Code == 810000:
			return kindQuota
		case ae.Status == 429 || ae.Code == 810001:
			return kindTransient
		case ae.Status >= 500:
			return kindTransient
		case ae.Status >= 400:
			return kindClient
		}
		return kindClient
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return kindNetwork
	}
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) && ne.Timeout() {
		return kindNetwork
	}
	msg := err.Error()
	if strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host") || strings.Contains(msg, "connection reset") || strings.Contains(msg, "EOF") || strings.Contains(msg, "TLS") {
		return kindNetwork
	}
	return kindNone
}
