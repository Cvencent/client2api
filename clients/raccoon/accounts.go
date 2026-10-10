package raccoon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"client2api/internal/core"
)

// Compile-time proof of which optional core interfaces this module serves.
// Deliberately NOT implemented (and why):
//   - core.TaskProvider / core.BatchProvider / core.CaptchaProvider /
//     core.HintProvider: the vendor exposes nothing of the sort.
//
// core.LoginProvider is implemented in login.go (loopback QR + SMS page);
// core.CheckinProvider is implemented in checkin.go (the desktop login-points
// grant, which is how the daily 300 credits are claimed).
// Deliberately NOT implemented (and why):
//   - core.TaskProvider / core.BatchProvider / core.CaptchaProvider /
//     core.HintProvider: the vendor exposes nothing of the sort.
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

// AccountFields describes the credential form shown in the panel.
func (c *Client) AccountFields(ctx context.Context) []core.FieldSpec {
	return []core.FieldSpec{
		{Key: "access_token", Label: "Access token", Type: "password", Required: true,
			Help: "The raccoon JWT. Copy it from the desktop client's credential store, or from a developer-tools network capture of any /api/web call."},
		{Key: "label", Label: "Label", Type: "text",
			Help: "Optional display name for this account."},
		{Key: "refresh_token", Label: "Refresh token", Type: "password",
			Help: "Optional but strongly recommended: without it the account cannot renew and will die when the access token expires (~3 h)."},
		{Key: "expires_at", Label: "Expires at", Type: "text",
			Help: "Optional millisecond timestamp. Leave blank to let the JWT `exp` claim decide."},
		{Key: "user_id", Label: "User id", Type: "text",
			Help: "Optional. Groups two credentials that belong to one account."},
		{Key: "nickname", Label: "Nickname", Type: "text"},
		{Key: "phone", Label: "Phone", Type: "text"},
		{Key: "office_identity", Label: "Org code", Type: "text",
			Help: "Leave blank for a personal account; otherwise the org code. Sent as X-Org-Code."},
		{Key: "device_id", Label: "Device id", Type: "text",
			Help: "Optional 32-hex id, sent as X-Client-Device-ID."},
	}
}

// Accounts renders the pool for the panel. It never errors on an empty pool.
func (c *Client) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	return c.pool.panelRecords(), nil
}

// AddAccount stores a pasted credential.
func (c *Client) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	tok := strings.TrimSpace(spec.Field("access_token"))
	if tok == "" {
		return core.AccountRecord{}, errors.New("raccoon: access_token is required")
	}
	if strings.ContainsAny(tok, " \t\r\n") {
		return core.AccountRecord{}, errors.New("raccoon: access_token must not contain whitespace")
	}
	a := account{
		credential: credential{
			AccessToken:    tok,
			RefreshToken:   strings.TrimSpace(spec.Field("refresh_token")),
			ExpiresAt:      flexString(strings.TrimSpace(spec.Field("expires_at"))),
			OfficeIdentity: strings.TrimSpace(spec.Field("office_identity")),
			UserID:         strings.TrimSpace(spec.Field("user_id")),
			Nickname:       strings.TrimSpace(spec.Field("nickname")),
			Phone:          strings.TrimSpace(spec.Field("phone")),
			DeviceID:       strings.TrimSpace(spec.Field("device_id")),
		},
		Label:  strings.TrimSpace(spec.Label),
		Origin: originStored,
	}
	if id := strings.TrimSpace(spec.ID); id != "" {
		a.ID = id
	}
	e := c.pool.put(a)
	if !spec.EnabledOr(true) {
		_ = c.pool.setEnabled(e.acct.id(), false)
	}
	return c.pool.record(e.acct.id()), nil
}

// RemoveAccount drops a stored credential. An account that comes from the
// module config is refused: the operator has to remove it from the config,
// otherwise the next restart would resurrect it.
func (c *Client) RemoveAccount(ctx context.Context, id string) error {
	e := c.pool.find(id)
	if e == nil {
		return fmt.Errorf("raccoon: unknown account %q", id)
	}
	if e.acct.Origin == originConfig || e.acct.Origin == originEnv {
		return fmt.Errorf("raccoon: account %q comes from the module config; remove it there", id)
	}
	if !c.pool.remove(id) {
		return fmt.Errorf("raccoon: unknown account %q", id)
	}
	return nil
}

// SetAccountEnabled parks or restores an account.
func (c *Client) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	return c.pool.setEnabled(id, enabled)
}

// ReviveAccount clears every runtime penalty without touching the credential.
func (c *Client) ReviveAccount(ctx context.Context, id string) error {
	e := c.pool.find(id)
	if e == nil {
		return fmt.Errorf("raccoon: unknown account %q", id)
	}
	c.pool.revive(e)
	return nil
}

// TestAccount makes ONE real, read-only call to prove the credential works.
// TestAccount sends ONE minimum-size real completion to prove the credential
// works, which is stronger than the old user-info read: it also proves the
// account can actually answer.  A refusal is a RESULT, not an error.
func (c *Client) TestAccount(ctx context.Context, id string) (core.TestResult, error) {
	e := c.pool.find(id)
	if e == nil {
		return core.TestResult{}, fmt.Errorf("raccoon: unknown account %q", id)
	}
	ctx, cancel := withTimeout(ctx, probeTimeout)
	defer cancel()
	start := time.Now()
	model := c.probeModelID()
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

// probeModelID names the first catalogue model, which is the cheapest probe
// target the platform exposes.
func (c *Client) probeModelID() string {
	ids := c.modelIDs()
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

// probeChat sends one minimum-size real completion through the given account
// and returns the first text it produced.
//
// A user-info read proves the token is accepted; only a completion proves the
// account can actually answer, which is what the panel's 测试 button asks.
// The account is pinned through chatWith so the probe never rotates onto a
// different credential than the one under test.
func (c *Client) probeChat(ctx context.Context, e *entry, model string) (string, error) {
	if strings.TrimSpace(model) == "" {
		return "", fmt.Errorf("raccoon: no model is available to probe with")
	}
	maxTokens := 16
	req := &core.ChatRequest{
		Model:     model,
		Messages:  []core.Message{{Role: "user", Content: "ping"}},
		MaxTokens: &maxTokens,
	}
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
		switch event.Type {
		case core.EventDelta:
			reply.WriteString(event.Delta)
		case core.EventError:
			if event.Err != nil {
				if reply.Len() > 0 {
					return reply.String(), nil
				}
				return "", event.Err
			}
		case core.EventDone:
			// keep draining until the stream really ends
		}
	}
	if reply.Len() == 0 {
		return "", fmt.Errorf("raccoon: %s answered without any text", model)
	}
	return reply.String(), nil
}

// RefreshAccount renews one account (or every refreshable account when id is
// empty). One account without a refresh token does not fail the whole call.
func (c *Client) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	var targets []*entry
	if strings.TrimSpace(id) == "" {
		targets = c.pool.all()
	} else {
		e := c.pool.find(id)
		if e == nil {
			return nil, fmt.Errorf("raccoon: unknown account %q", id)
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

// refreshAccount renews ONE entry's access token and writes the result back,
// including the new expiry.
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

// ---- credential import -------------------------------------------------

// Discover reports the operator-declared credential files that exist and can
// be imported. The module does NOT guess at vendor client storage paths; see
// the README for why.
func (c *Client) Discover(ctx context.Context) ([]core.DiscoveredCredential, error) {
	var out []core.DiscoveredCredential
	for _, p := range c.cfg.credentialPaths() {
		entry := core.DiscoveredCredential{Path: p, Kind: "raccoon-credential"}
		data, err := os.ReadFile(p)
		if err != nil {
			entry.Note = "not readable: " + err.Error()
			out = append(out, entry)
			continue
		}
		creds, err := parseCredentialBundle(data)
		if err != nil {
			entry.Note = "not a raccoon credential file: " + err.Error()
			out = append(out, entry)
			continue
		}
		entry.Importable = true
		if n := len(creds); n == 1 {
			entry.Label = creds[0].displayName()
		} else {
			entry.Label = fmt.Sprintf("%d credentials", n)
		}
		entry.Imported = c.allImported(creds)
		if entry.Imported {
			entry.Note = "already imported"
		}
		out = append(out, entry)
	}
	return out, nil
}

func (c *Client) allImported(creds []credential) bool {
	if len(creds) == 0 {
		return false
	}
	for _, cr := range creds {
		if c.pool.find(accountIDFor(cr)) == nil {
			return false
		}
	}
	return true
}

// Import reads the given credential files (or every discovered one when
// all is true) into the pool.
func (c *Client) Import(ctx context.Context, paths []string, all bool) ([]core.AccountRecord, error) {
	targets := paths
	if all || len(targets) == 0 {
		targets = c.cfg.credentialPaths()
	}
	if len(targets) == 0 {
		return nil, errors.New("raccoon: no credential_paths configured to import from")
	}
	var out []core.AccountRecord
	var firstErr error
	for _, p := range targets {
		data, err := os.ReadFile(p)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		creds, err := parseCredentialBundle(data)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", p, err)
			}
			continue
		}
		for _, cr := range creds {
			e := c.pool.put(account{
				credential: cr,
				Label:      cr.displayName(),
				Origin:     originImport,
			})
			out = append(out, c.pool.record(e.acct.id()))
		}
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func accountIDFor(cr credential) string {
	return account{credential: cr}.id()
}

// parseCredentialBundle accepts the several shapes an operator is likely to
// paste: a single credential object, an array of them, or an object with a
// `credentials`/`accounts` array.
func parseCredentialBundle(data []byte) ([]credential, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, errors.New("empty file")
	}
	var single credential
	if err := json.Unmarshal(data, &single); err == nil && strings.TrimSpace(single.AccessToken) != "" {
		return []credential{single}, nil
	}
	var list []credential
	if err := json.Unmarshal(data, &list); err == nil {
		if out := usableCredentials(list); len(out) > 0 {
			return out, nil
		}
	}
	var wrapper struct {
		Credentials []credential `json:"credentials"`
		Accounts    []credential `json:"accounts"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil {
		if out := usableCredentials(wrapper.Credentials); len(out) > 0 {
			return out, nil
		}
		if out := usableCredentials(wrapper.Accounts); len(out) > 0 {
			return out, nil
		}
	}
	return nil, errors.New("no access_token found")
}

func usableCredentials(in []credential) []credential {
	out := make([]credential, 0, len(in))
	for _, cr := range in {
		if strings.TrimSpace(cr.AccessToken) != "" {
			out = append(out, cr)
		}
	}
	return out
}

// panelRecords renders the pool for the panel, including parked accounts.
func (p *pool) panelRecords() []core.AccountRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := make([]core.AccountRecord, 0, len(p.entries))
	for _, e := range p.entries {
		p.usableLocked(e, now)
		fields := map[string]any{
			"origin":      e.acct.Origin,
			"failures":    e.fails,
			"refreshable": e.acct.cred().refreshable(),
			"removable":   e.acct.Origin == originStored || e.acct.Origin == originImport,
		}
		if e.acct.UserID != "" {
			fields["user_id"] = e.acct.UserID
		}
		if e.acct.Nickname != "" {
			fields["nickname"] = e.acct.Nickname
		}
		if e.acct.Phone != "" {
			fields["phone"] = maskPhone(e.acct.Phone)
		}
		if e.acct.OfficeIdentity != "" {
			fields["office_identity"] = e.acct.OfficeIdentity
		}
		if ms, ok := e.acct.cred().expiresAtMs(); ok {
			fields["expires_in"] = int64(time.Until(time.UnixMilli(ms)).Seconds())
		}
		if !e.until.IsZero() && e.until.After(now) {
			fields["cooldown_until"] = e.until.UTC().Format(time.RFC3339)
		}
		out = append(out, core.AccountRecord{
			ID:       e.acct.id(),
			Label:    panelLabel(e.acct),
			Enabled:  !e.acct.Disabled,
			State:    e.state,
			Note:     e.note,
			Fields:   fields,
			Identity: e.acct.cred().identity(),
		})
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

// classifyErr maps an error onto a pool failure kind. A non-retryable kind
// means the fault was not the credential's, so no cooldown is applied.
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
		case ae.Status == 401 || ae.Status == 403 || ae.Code == refreshCodeDead:
			return kindAuth
		case ae.Status == 402:
			return kindQuota
		case ae.Status == 429:
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
	if strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "TLS") {
		return kindNetwork
	}
	return kindNone
}
