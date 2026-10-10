package openaicompat

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// maxRotate bounds how many providers one chat request may try.  Rotation is
// for "this key is out of quota / throttled / rejected", not a retry loop.
const maxRotate = 3

// maxErrorBytes bounds how much of an error body is read into memory.
const maxErrorBytes = 64 << 10

// maxModelsBytes bounds the catalogue read.
const maxModelsBytes = 8 << 20

// Client is the module.  Every provider is an account in the pool.
type Client struct {
	instanceName string
	deps         core.Deps
	cfg          Config
	cfgErr       error

	// configProviders is the config-file half of the pool.  The panel-owned
	// half lives in accounts.json; reloadProviders merges the two into
	// cfg.Providers, so providerFor and Models see one flat list.
	configProviders []ProviderConfig

	pool   *providerPool
	models *struct{}
	now    func() time.Time
	// affinity pins a conversation to the provider account that served it.
	// A provider here is one credential, so a binding keeps a repeated
	// conversation on the upstream that already warmed its prompt cache.
	affinity *core.Affinity

	mu      sync.Mutex
	lastErr string
}

func init() {
	core.Register(clientName, New)
	core.RegisterSourceFactory(func(name string, cfg core.SourceConfig, deps core.Deps) (core.SourceClient, error) {
		return newSource(name, cfg, deps)
	})
}

// New builds the module.
func New(deps core.Deps) (core.Client, error) {
	c := &Client{
		deps:     deps,
		now:      time.Now,
		pool:     newProviderPool(),
		models:   &struct{}{},
		affinity: core.NewAffinity(0),
	}
	c.affinity.StartGC()
	cfg, err := parseConfig(deps.Config)
	if err != nil {
		c.cfgErr = err
		deps.Log("%s: %v (using built-in defaults)", clientName, core.Redact(err.Error()))
	}
	c.cfg = cfg
	c.configProviders = cfg.Providers
	c.reloadProviders()
	return c, nil
}

// ensure fills fields a hand-assembled (test) client may be missing.
func (c *Client) ensure() {
	if c.now == nil {
		c.now = time.Now
	}
	if c.pool == nil {
		c.pool = newProviderPool()
	}
	if c.models == nil {
		c.models = &struct{}{}
	}
	if c.affinity == nil {
		c.affinity = core.NewAffinity(0)
		c.affinity.StartGC()
	}
}

func (c *Client) Name() string {
	if c.instanceName != "" {
		return c.instanceName
	}
	return clientName
}

func (c *Client) httpClient() *http.Client {
	if c.deps.HTTPClient != nil {
		return c.deps.HTTPClient
	}
	return defaultHTTPClient
}

// noteError remembers the last error for Status.
func (c *Client) noteError(msg string) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return
	}
	msg = truncate(core.Redact(msg), 300)
	c.mu.Lock()
	changed := msg != c.lastErr
	c.lastErr = msg
	c.mu.Unlock()
	if changed {
		c.deps.Log("%s: %s", clientName, msg)
	}
}

// --- credentials -----------------------------------------------------------

// buildCredentials assembles the provider pool from the config.
func (c *Client) buildCredentials() []providerRecord {
	recs := make([]providerRecord, 0, len(c.cfg.Providers))
	for _, p := range c.cfg.Providers {
		recs = append(recs, providerRecord{
			ID:       p.ID,
			Label:    firstNonEmpty(p.Label, p.ID),
			APIKey:   p.APIKey,
			BaseURL:  p.base(),
			Models:   p.Models,
			Headers:  p.ExtraHeaders,
			Disabled: p.Disabled,
			Source:   p.source(),
		})
	}
	return recs
}

// reloadProviders rebuilds the merged provider list from the config-file rows
// plus the panel-owned rows, then republishes the pool.  A panel row whose id
// collides with a config row is dropped: the config file is the operator's
// durable copy and the panel must never silently shadow it.
func (c *Client) reloadProviders() {
	stored := c.loadProviders()
	merged := make([]ProviderConfig, 0, len(c.configProviders)+len(stored))
	seen := make(map[string]bool, len(c.configProviders)+len(stored))
	for _, p := range c.configProviders {
		p.Source = sourceConfig
		merged = append(merged, p)
		seen[strings.ToLower(p.ID)] = true
	}
	for _, s := range stored {
		if seen[strings.ToLower(s.ID)] {
			continue
		}
		p := storedToConfig(s)
		if p.BaseURL == "" {
			p.BaseURL = builtinBaseURL(p.ID)
		}
		if p.BaseURL == "" {
			continue
		}
		p.Source = sourcePanel
		merged = append(merged, p)
		seen[strings.ToLower(p.ID)] = true
	}
	c.cfg.Providers = merged
	c.pool.reload(c.buildCredentials())
}

// providerByID returns the merged provider row for an id.
func (c *Client) providerByID(id string) (ProviderConfig, bool) {
	for _, p := range c.cfg.Providers {
		if strings.EqualFold(p.ID, id) {
			return p, true
		}
	}
	return ProviderConfig{}, false
}

// providerFor resolves a requested model of the form "<provider>/<model>" to
// the matching configured provider.  The model id keeps any further slashes.
func (c *Client) providerFor(model string) (ProviderConfig, string, bool) {
	model = strings.TrimSpace(model)
	prefix, rest, ok := strings.Cut(model, "/")
	if !ok || prefix == "" || rest == "" {
		return ProviderConfig{}, "", false
	}
	for _, p := range c.cfg.Providers {
		if strings.EqualFold(p.ID, prefix) {
			return p, rest, true
		}
	}
	return ProviderConfig{}, "", false
}

// --- chat ------------------------------------------------------------------

// Chat picks a provider, sends the request and returns the response stream.
func (c *Client) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	c.ensure()
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, fmt.Errorf("%w: nil chat request", core.ErrUnsupported)
	}
	prov, model, ok := c.providerFor(req.Model)
	if !ok {
		return nil, fmt.Errorf("%w: model %q must be qualified as <provider>/<model>", core.ErrUnsupported, req.Model)
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("%w: a chat request needs at least one message", core.ErrUnsupported)
	}
	if prov.Disabled {
		return nil, fmt.Errorf("%w: provider %q is disabled in the module config", core.ErrUnsupported, prov.ID)
	}
	body, err := buildChatBody(c.cfg, prov, model, req)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rec, ok := c.pool.byID(prov.ID)
	if !ok {
		return nil, fmt.Errorf("%w: provider %q is not configured", core.ErrNotConfigured, prov.ID)
	}
	if err := req.AcquireAccountSlot(rec.ID); err != nil {
		return nil, err
	}
	if key := core.ConversationKeyOf(req); key != "" {
		c.affinity.Bind(key, rec.ID)
	}
	core.NoteServedBy(req, rec.ID)
	stream, err := c.doChat(ctx, prov, rec.ID, body)
	if err != nil {
		req.ReleaseAccountSlot()
		return nil, err
	}
	return stream, nil
}

// doChat issues one POST /chat/completions against a provider.
func (c *Client) doChat(ctx context.Context, prov ProviderConfig, accountID string, body []byte) (core.Stream, error) {
	reqCtx, cancel := context.WithTimeout(ctxOrBackground(ctx), c.cfg.chatTimeout())
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, prov.chatURL(), bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("openai-compat: build request: %w", err)
	}
	prov.applyHeaders(httpReq)

	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		cancel()
		return nil, c.classifyUpstream(accountID, err)
	}
	if resp.StatusCode != http.StatusOK {
		raw := readLimited(resp.Body, maxErrorBytes)
		_ = resp.Body.Close()
		cancel()
		return nil, c.classifyHTTP("chat", accountID, resp.StatusCode, raw)
	}
	// The stream holds the slot the caller took; closing it returns the slot.
	return newChatStream(c, resp, prov, accountID, cancel), nil
}

// --- status ----------------------------------------------------------------

// Status performs no network I/O.
func (c *Client) Status(ctx context.Context) core.Status {
	c.ensure()
	now := c.now()
	st := core.Status{
		Name:      clientName,
		UpdatedAt: now,
		Accounts:  c.pool.statuses(now),
		Models:    c.modelIDs(),
	}
	st.Label = "onmiRoute"
	st.Ready = c.pool.ready(now) > 0
	st.Detail = c.pool.summary(now)
	if c.cfgErr != nil {
		st.Detail += "; config error: " + truncate(core.Redact(c.cfgErr.Error()), 300)
	}
	if msg := c.lastError(); msg != "" {
		st.Detail += "; last error: " + msg
	}
	return st
}

// lastError reads the memoized error line.
func (c *Client) lastError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}

// --- models ----------------------------------------------------------------

// Models serves the aggregated catalogue: each configured provider contributes
// its models, qualified with the provider id ("groq/llama-3.3-70b-versatile").
// The list never blocks on the network.
func (c *Client) Models(ctx context.Context) ([]core.Model, error) {
	c.ensure()
	out := make([]core.Model, 0, 32)
	seen := make(map[string]bool, 32)
	for _, p := range c.cfg.Providers {
		if p.Disabled {
			continue
		}
		for _, id := range p.modelsFor(c.cfg.ExtraModels) {
			qualified := p.ID + "/" + id
			if seen[qualified] {
				continue
			}
			seen[qualified] = true
			m := core.Model{
				ID:      qualified,
				OwnedBy: p.ID,
				Extra:   map[string]any{"vendor": p.ID, "source": "config"},
			}
			out = append(out, m)
		}
	}
	return out, nil
}

// ModelMaxOutputTokens is answered from the static catalogue only.  A model
// the catalogue does not know is answered with ok=false, which is a normal
// answer, not a failure.
func (c *Client) ModelMaxOutputTokens(ctx context.Context, model string) (int, bool) {
	c.ensure()
	_, _, ok := c.providerFor(model)
	if !ok {
		return 0, false
	}
	return 0, false
}

// --- status helpers ----------------------------------------------------------

// modelIDs is the bare id list the panel renders.
func (c *Client) modelIDs() []string {
	list, err := c.Models(context.Background())
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.ID)
	}
	return out
}

// --- small helpers ----------------------------------------------------------

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func readLimited(r io.Reader, n int64) []byte {
	body, _ := io.ReadAll(io.LimitReader(r, n))
	return body
}

func ctxOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

var defaultHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
	},
}
