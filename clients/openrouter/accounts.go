package openrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"client2api/internal/core"
)

// This file implements core.AccountManager.  Discover/Import (the credential
// importer half) live in credential.go.
//
// The pool's rule is that a row is listed only when the panel can act on it:
// RemoveAccount and SetAccountEnabled really do work for an imported or
// panel-added key, and a key that arrived from the module config or the
// environment is reported as unremovable HERE (with an explanation) because the
// durable copy lives somewhere this module must not edit.

// AccountFields describes what the panel's "add account" form collects.
func (c *Client) AccountFields(ctx context.Context) []core.FieldSpec {
	return []core.FieldSpec{
		{
			Key:         "api_key",
			Label:       "API key",
			Type:        "password",
			Required:    true,
			Placeholder: "sk-or-v1-…",
			Help:        "An OpenRouter API key (openrouter.ai/keys). It is written to this module's own data directory, never to the main config.",
		},
		{
			Key:         "label",
			Label:       "Label",
			Type:        "text",
			Placeholder: "personal key",
			Help:        "Optional display name. Defaults to a masked form of the key.",
		},
		{
			Key:   "id",
			Label: "ID",
			Type:  "text",
			Help:  "Optional stable id. Defaults to a fingerprint of the key, so re-adding the same key updates the existing row instead of duplicating it.",
		},
	}
}

// DirectKeyField implements core.DirectKeyProvider.
//
// A key the PKCE login mints is scoped to this application, so OpenRouter
// applies THIS APP's rate and credit limits to it.  A key from the operator's
// own account page is limited by that account instead, and free-model quota
// is the whole reason this module exists -- so the pasted path is offered
// right next to the login button rather than hidden behind the generic form.
func (c *Client) DirectKeyField(ctx context.Context) string { return "api_key" }

var _ core.DirectKeyProvider = (*Client)(nil)

// Accounts lists every credential the module knows about.  It never fails just
// because the pool is empty.
func (c *Client) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	c.ensure()
	return c.pool.records(c.now()), nil
}

// AddAccount stores a key pasted into the panel.
func (c *Client) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	c.ensure()
	key := strings.TrimSpace(spec.Fields["api_key"])
	if key == "" {
		return core.AccountRecord{}, fmt.Errorf("an OpenRouter API key is required")
	}
	if strings.ContainsAny(key, " \t\r\n") {
		return core.AccountRecord{}, fmt.Errorf("the API key contains whitespace; paste it as a single unbroken token")
	}
	id := strings.TrimSpace(spec.Fields["id"])
	if id == "" {
		id = accountIDFor(key)
	}
	if existing, ok := c.pool.byID(id); ok {
		if existing.Source == sourceConfig || existing.Source == sourceEnv {
			return core.AccountRecord{}, fmt.Errorf("account %q already comes from the module configuration or the environment; change it there instead", id)
		}
	}
	enabled := true
	if spec.Enabled != nil {
		enabled = *spec.Enabled
	}
	label := firstNonEmpty(strings.TrimSpace(spec.Fields["label"]), strings.TrimSpace(spec.Label), "panel key")
	c.pool.upsert(accountRecord{
		ID:      id,
		Label:   label,
		APIKey:  key,
		Source:  sourcePanel,
		AddedAt: c.now().Format(time.RFC3339),
	})
	c.pool.setEnabled(id, enabled)
	c.persistCredentials()
	c.persistState()

	rec, ok := c.panelRecord(id)
	if !ok {
		return core.AccountRecord{}, fmt.Errorf("account %q vanished after it was stored", id)
	}
	return rec, nil
}

// RemoveAccount deletes a stored key.  A config/env credential is refused with
// a message that says where the real copy lives: silently removing it would
// make it reappear on the next restart.
func (c *Client) RemoveAccount(ctx context.Context, id string) error {
	c.ensure()
	rec, ok := c.pool.byID(id)
	if !ok {
		return fmt.Errorf("account %q not found", id)
	}
	switch rec.Source {
	case sourceConfig:
		return fmt.Errorf("account %q comes from clients.openrouter (api_key or accounts); remove it there instead", id)
	case sourceEnv:
		return fmt.Errorf("account %q comes from the %s environment variable; unset it there instead", id, envAPIKey)
	}
	c.pool.remove(id)
	c.persistCredentials()
	c.persistState()
	return nil
}

// SetAccountEnabled parks or restores a credential without touching its key.
func (c *Client) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	c.ensure()
	if _, ok := c.pool.byID(id); !ok {
		return fmt.Errorf("account %q not found", id)
	}
	c.pool.setEnabled(id, enabled)
	c.persistState()
	return nil
}

// TestAccount runs one tiny completion through the credential.
//
// A refusal is a RESULT, not an error: the panel has to show *why* a key is no
// good, and only an unknown id is a programming mistake.
func (c *Client) TestAccount(ctx context.Context, id string) (res core.TestResult, err error) {
	c.ensure()
	rec, ok := c.pool.byID(id)
	if !ok {
		return core.TestResult{}, fmt.Errorf("account %q not found", id)
	}
	res = core.TestResult{AccountID: id}
	start := c.now()
	defer func() { res.ElapsedMS = c.now().Sub(start).Milliseconds() }()

	if rec.APIKey == "" {
		res.Error = "this account has no API key"
		return res, nil
	}
	model := c.probeModel()
	res.Model = model
	body, err := json.Marshal(oaiChatRequest{
		Model:  model,
		Stream: true,
		Messages: []oaiMessage{
			{Role: "user", Content: "Reply with the single word: pong"},
		},
	})
	if err != nil {
		res.Error = "could not build the probe request"
		return res, nil
	}
	probeCtx, cancel := context.WithTimeout(ctxOrBackground(ctx), c.cfg.modelsTimeout())
	defer cancel()

	stream, err := c.openChatProbe(probeCtx, rec, body)
	if err != nil {
		res.Error = truncate(core.Redact(err.Error()), 300)
		return res, nil
	}
	text, _, _, _, _, derr := drainStream(stream)
	res.Reply = truncate(text, 200)
	if derr != nil {
		res.Error = truncate(core.Redact(derr.Error()), 300)
		return res, nil
	}
	if strings.TrimSpace(text) == "" {
		res.Error = "the vendor accepted the request but returned no text"
		return res, nil
	}
	res.OK = true
	return res, nil
}

// RefreshAccount re-validates a credential against `GET /key`.
//
// There is no token to renew here (an OpenRouter key does not expire on its
// own), so "refresh" means "ask the vendor whether this key still works, and
// lift a stale park if it does".  An empty id means every account, and one bad
// key is not a reason to fail the whole call.
func (c *Client) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	c.ensure()
	id = strings.TrimSpace(id)
	var targets []accountRecord
	if id == "" {
		targets = c.pool.snapshot()
	} else {
		rec, ok := c.pool.byID(id)
		if !ok {
			return nil, fmt.Errorf("account %q not found", id)
		}
		targets = []accountRecord{rec}
	}
	out := make([]core.RefreshResult, 0, len(targets))
	for _, rec := range targets {
		res := core.RefreshResult{AccountID: rec.ID}
		if rec.APIKey == "" {
			res.Error = "this account has no API key"
			out = append(out, res)
			continue
		}
		body, status, err := c.getJSON(ctx, rec, c.cfg.keyURL())
		switch {
		case err != nil:
			res.Error = truncate(core.Redact(err.Error()), 300)
		case status != http.StatusOK:
			res.Error = errorTextOf(body)
			c.noteFailure(rec.ID, classifyFailure(status, 0, res.Error), res.Error)
		default:
			res.OK = true
			c.noteSuccess(rec)
		}
		out = append(out, res)
	}
	return out, nil
}

// panelRecord renders one stored credential the way the pool renders all of
// them, so the panel sees identical fields from Accounts and AddAccount.
func (c *Client) panelRecord(id string) (core.AccountRecord, bool) {
	for _, rec := range c.pool.records(c.now()) {
		if rec.ID == id {
			return rec, true
		}
	}
	return core.AccountRecord{}, false
}

// openChatProbe is openChat for an explicit credential.  It does NOT take an
// in-flight slot: a panel probe is a diagnostic, not a served request.
func (c *Client) openChatProbe(ctx context.Context, rec accountRecord, body []byte) (core.Stream, error) {
	return c.doChat(ctx, rec, body, false)
}

func ctxOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
