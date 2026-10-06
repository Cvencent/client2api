package openaicompat

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"client2api/internal/core"
)

// This file implements core.AccountManager.  Each provider is an account, and
// the rule mirrors the rest of the project: a row the panel can act on is a
// row this module owns.  A provider written into the module config is shown
// but reported as unremovable, because its durable copy lives in the config
// file the panel must not edit.

// AccountFields describes what the panel's "add provider" form collects.
func (c *Client) AccountFields(ctx context.Context) []core.FieldSpec {
	return []core.FieldSpec{
		{
			Key:         "provider",
			Label:       "Provider id",
			Type:        "text",
			Required:    true,
			Options:     builtinProviderIDs(),
			Placeholder: "groq",
			Help:        "The routing prefix under this module (<provider>/<model>).  Pick a known vendor to fill in its base URL, or type an id for any other OpenAI-compatible source.",
		},
		{
			Key:      "api_key",
			Label:    "API key",
			Type:     "password",
			Required: true,
			Help:     "The bearer token for this provider.  It is written to this module's own data directory, never to the main config.",
		},
		{
			Key:         "base_url",
			Label:       "Base URL",
			Type:        "text",
			Placeholder: "https://api.example.com/v1",
			Help:        "Optional OpenAI-compatible API root.  Leave blank for a known provider, or fill it in for a vendor this module does not ship a default for.",
		},
		{
			Key:         "models",
			Label:       "Models",
			Type:        "textarea",
			Placeholder: "llama-3.3-70b-versatile, openai/gpt-oss-120b",
			Help:        "Optional comma- or newline-separated model ids served by this key.  Leave blank to use the module's built-in catalogue for the provider.",
		},
		{
			Key:         "label",
			Label:       "Label",
			Type:        "text",
			Placeholder: "personal key",
			Help:        "Optional display name.  Defaults to the provider id.",
		},
	}
}

var _ core.AccountManager = (*Client)(nil)

// KeyPageURLs implements core.KeyPageProvider.  The panel shows the link for
// whichever provider is selected in the add form, which is the difference
// between "paste a key" and "go get a key and paste it".
func (c *Client) KeyPageURLs(ctx context.Context) map[string]string {
	out := make(map[string]string, len(keyPages))
	for id, u := range keyPages {
		out[id] = u
	}
	return out
}

var _ core.KeyPageProvider = (*Client)(nil)

// Accounts lists every provider the module knows about.  It never fails just
// because the pool is empty.
func (c *Client) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	c.ensure()
	return c.pool.records(c.now()), nil
}

// AddAccount stores a provider pasted into the panel.
func (c *Client) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	c.ensure()
	id := firstNonEmpty(strings.TrimSpace(spec.Fields["provider"]), strings.TrimSpace(spec.ID))
	if id == "" {
		return core.AccountRecord{}, fmt.Errorf("a provider id is required")
	}
	if strings.ContainsAny(id, " \t\r\n") {
		return core.AccountRecord{}, fmt.Errorf("the provider id must not contain whitespace")
	}
	if existing, ok := c.pool.byID(id); ok && existing.Source == sourceConfig {
		return core.AccountRecord{}, fmt.Errorf("provider %q already comes from the module configuration; change it there instead", id)
	}

	baseURL := strings.TrimRight(strings.TrimSpace(spec.Fields["base_url"]), "/")
	if baseURL == "" {
		baseURL = builtinBaseURL(id)
	}
	if baseURL == "" {
		return core.AccountRecord{}, fmt.Errorf("provider %q has no built-in base URL; enter one in the base_url field", id)
	}

	// A key is required unless the upstream is on this machine: a loopback
	// base URL is the one case where the service itself answers keyless.
	key := strings.TrimSpace(spec.Fields["api_key"])
	if key == "" && !isLoopbackBase(baseURL) {
		return core.AccountRecord{}, fmt.Errorf("an API key is required")
	}
	if strings.ContainsAny(key, " \t\r\n") {
		return core.AccountRecord{}, fmt.Errorf("the API key contains whitespace; paste it as a single unbroken token")
	}

	row := storedProvider{
		ID:      id,
		Label:   firstNonEmpty(strings.TrimSpace(spec.Fields["label"]), strings.TrimSpace(spec.Label), id),
		BaseURL: baseURL,
		APIKey:  key,
		Models:  splitModels(spec.Fields["models"]),
	}
	if spec.Enabled != nil {
		row.Disabled = !*spec.Enabled
	}
	if err := c.upsertStored(row); err != nil {
		return core.AccountRecord{}, err
	}
	rec, ok := c.panelRecord(id)
	if !ok {
		return core.AccountRecord{}, fmt.Errorf("provider %q vanished after it was stored", id)
	}
	return rec, nil
}

// RemoveAccount deletes a stored provider.  A config-sourced provider is
// refused with a message that says where the real copy lives: silently removing
// it would make it reappear on the next restart.
func (c *Client) RemoveAccount(ctx context.Context, id string) error {
	c.ensure()
	id = strings.TrimSpace(id)
	rec, ok := c.pool.byID(id)
	if !ok {
		return fmt.Errorf("provider %q not found", id)
	}
	if rec.Source == sourceConfig {
		return fmt.Errorf("provider %q comes from the module configuration; remove it there instead", rec.ID)
	}
	rows := c.loadProviders()
	kept := rows[:0]
	for _, r := range rows {
		if strings.EqualFold(r.ID, id) {
			continue
		}
		kept = append(kept, r)
	}
	if err := c.saveProviders(kept); err != nil {
		return err
	}
	c.reloadProviders()
	return nil
}

// SetAccountEnabled parks or restores a provider without touching its key.
func (c *Client) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	c.ensure()
	id = strings.TrimSpace(id)
	rec, ok := c.pool.byID(id)
	if !ok {
		return fmt.Errorf("provider %q not found", id)
	}
	if rec.Source == sourceConfig {
		return fmt.Errorf("provider %q comes from the module configuration; change its \"disabled\" field there instead", rec.ID)
	}
	rows := c.loadProviders()
	found := false
	for i := range rows {
		if strings.EqualFold(rows[i].ID, id) {
			rows[i].Disabled = !enabled
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("provider %q is not stored in this module; re-add it from the panel", id)
	}
	if err := c.saveProviders(rows); err != nil {
		return err
	}
	c.reloadProviders()
	return nil
}

// TestAccount runs one tiny completion through the provider's key.  A refusal
// is a result, not an error: the panel has to show why a key is no good.
func (c *Client) TestAccount(ctx context.Context, id string) (core.TestResult, error) {
	c.ensure()
	id = strings.TrimSpace(id)
	rec, ok := c.pool.byID(id)
	if !ok {
		return core.TestResult{}, fmt.Errorf("provider %q not found", id)
	}
	prov, ok := c.providerByID(rec.ID)
	if !ok {
		return core.TestResult{}, fmt.Errorf("provider %q not found", rec.ID)
	}
	return c.probeProvider(ctx, prov, rec), nil
}

// RefreshAccount re-validates a provider by asking its /models endpoint.
//
// There is no token to renew here (an API key does not expire on its own), so
// "refresh" means "ask the vendor whether this key still works".  An empty id
// means every provider, and one bad key is not a reason to fail the whole
// call.  A vendor without a /models endpoint reports that as a per-provider
// error rather than failing the batch.
func (c *Client) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	c.ensure()
	id = strings.TrimSpace(id)
	var targets []providerRecord
	if id == "" {
		targets = c.pool.snapshot()
	} else {
		rec, ok := c.pool.byID(id)
		if !ok {
			return nil, fmt.Errorf("provider %q not found", id)
		}
		targets = []providerRecord{rec}
	}
	out := make([]core.RefreshResult, 0, len(targets))
	for _, rec := range targets {
		res := core.RefreshResult{AccountID: rec.ID}
		if rec.APIKey == "" && !isLoopbackBase(rec.BaseURL) {
			res.Error = "this provider has no API key"
			out = append(out, res)
			continue
		}
		prov, ok := c.providerByID(rec.ID)
		if !ok {
			res.Error = "this provider is no longer configured"
			out = append(out, res)
			continue
		}
		if errText := c.checkProvider(ctx, prov, rec); errText != "" {
			res.Error = errText
		} else {
			res.OK = true
		}
		out = append(out, res)
	}
	return out, nil
}

// --- helpers ----------------------------------------------------------------

// upsertStored writes one panel-owned row, replacing any row with the same id.
func (c *Client) upsertStored(row storedProvider) error {
	rows := c.loadProviders()
	replaced := false
	for i := range rows {
		if strings.EqualFold(rows[i].ID, row.ID) {
			rows[i] = row
			replaced = true
			break
		}
	}
	if !replaced {
		rows = append(rows, row)
	}
	if err := c.saveProviders(rows); err != nil {
		return err
	}
	c.reloadProviders()
	return nil
}

// panelRecord renders one provider the way the pool renders all of them, so the
// panel sees identical fields from Accounts and AddAccount.
func (c *Client) panelRecord(id string) (core.AccountRecord, bool) {
	for _, rec := range c.pool.records(c.now()) {
		if strings.EqualFold(rec.ID, id) {
			return rec, true
		}
	}
	return core.AccountRecord{}, false
}

// probeProvider issues one short streaming completion for a provider.
func (c *Client) probeProvider(ctx context.Context, prov ProviderConfig, rec providerRecord) core.TestResult {
	res := core.TestResult{AccountID: rec.ID}
	start := c.now()
	defer func() { res.ElapsedMS = c.now().Sub(start).Milliseconds() }()

	if rec.APIKey == "" && !isLoopbackBase(rec.BaseURL) {
		res.Error = "this provider has no API key"
		return res
	}
	prov.APIKey = rec.APIKey
	prov.Disabled = false

	model := c.probeModel(prov)
	res.Model = model
	if model == "" {
		res.Error = "this provider has no model to probe with; add one to its models list"
		return res
	}

	body, err := buildChatBody(c.cfg, prov, model, &core.ChatRequest{
		Model:    model,
		Stream:   true,
		Messages: []core.Message{{Role: "user", Content: "Reply with the single word: pong"}},
	})
	if err != nil {
		res.Error = truncate(core.Redact(err.Error()), 300)
		return res
	}

	probeCtx, cancel := context.WithTimeout(ctxOrBackground(ctx), c.cfg.modelsTimeout())
	defer cancel()
	stream, err := c.doChat(probeCtx, prov, rec.ID, body)
	if err != nil {
		res.Error = truncate(core.Redact(err.Error()), 300)
		return res
	}
	text, _, _, _, _, derr := drainStream(stream)
	res.Reply = truncate(text, 200)
	if derr != nil {
		res.Error = truncate(core.Redact(derr.Error()), 300)
		return res
	}
	if strings.TrimSpace(text) == "" {
		res.Error = "the vendor accepted the request but returned no text"
		return res
	}
	res.OK = true
	return res
}

// checkProvider asks GET /models with the provider's key; an empty return means
// the key was accepted.
func (c *Client) checkProvider(ctx context.Context, prov ProviderConfig, rec providerRecord) string {
	prov.APIKey = rec.APIKey
	reqCtx, cancel := context.WithTimeout(ctxOrBackground(ctx), c.cfg.modelsTimeout())
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, prov.base()+"/models", nil)
	if err != nil {
		return truncate(core.Redact(err.Error()), 300)
	}
	prov.applyHeaders(httpReq)
	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return truncate(core.Redact(err.Error()), 300)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := readLimited(resp.Body, maxErrorBytes)
		msg := errorTextOf(raw)
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return truncate(core.Redact(msg), 300)
	}
	return ""
}

// probeModel picks the cheapest probe target: the first model the provider
// advertises, falling back to the built-in catalogue.
func (c *Client) probeModel(prov ProviderConfig) string {
	for _, m := range prov.modelsFor(c.cfg.ExtraModels) {
		if strings.TrimSpace(m) != "" {
			return strings.TrimSpace(m)
		}
	}
	return ""
}

// splitModels parses a comma- or newline-separated model list from a form.
func splitModels(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// builtinProviderIDs is the sorted list of providers with a shipped base URL.
func builtinProviderIDs() []string {
	out := make([]string, 0, len(builtinProviders))
	for id := range builtinProviders {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
