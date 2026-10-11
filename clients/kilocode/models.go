package kilocode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// maxModelsBytes bounds the catalogue read.  The live response is a few hundred
// KB; the cap exists so a broken or hostile endpoint cannot exhaust memory.
const maxModelsBytes = 32 << 20

// curatedModel is one row of the built-in catalogue.
//
// Context and MaxOutput are the vendor's own published numbers, recorded from a
// live `GET /models` probe.  They are the cold-start answer: a fresh process can
// describe the common FREE models and answer ModelMaxOutputTokens before it has
// ever spoken to the network.
type curatedModel struct {
	ID        string
	Context   int
	MaxOutput int
}

// curatedModels is the slice of the gateway's free catalogue that is genuinely
// chat-capable, recorded on 2026-10-11.  The live `GET /models` response always
// wins when it is available; this table only covers a total outage.  The
// safety-classifier and music-generation free entries are deliberately absent:
// they are not chat models and would make a bad probe target.
var curatedModels = []curatedModel{
	{ID: "kilo-auto/free", Context: 256000, MaxOutput: 32768},
	{ID: "stepfun/step-5-preview-free", Context: 1000000, MaxOutput: 64000},
	{ID: "nvidia/nemotron-3-ultra-550b-a55b:free", Context: 1000000, MaxOutput: 65536},
	{ID: "nvidia/nemotron-3-super-120b-a12b:free", Context: 262144, MaxOutput: 235929},
	{ID: "nvidia/nemotron-3.5-lightning:free", Context: 1000000, MaxOutput: 65536},
	{ID: "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free", Context: 256000, MaxOutput: 65536},
	{ID: "dots-studio/dots-3-note-preview:free", Context: 512000, MaxOutput: 460800},
	{ID: "thinkingmachines/inkling-small:free", Context: 1048576, MaxOutput: 262144},
	{ID: "poolside/laguna-s-2.1:free", Context: 262144, MaxOutput: 32768},
	{ID: "poolside/laguna-xs-2.1:free", Context: 262144, MaxOutput: 32768},
	{ID: "cohere/north-mini-code:free", Context: 256000, MaxOutput: 64000},
	{ID: "liquid/lfm-2.5-2.6b:free", Context: 65536, MaxOutput: 8192},
	{ID: "inclusionai/ling-3.1-flash", Context: 262144, MaxOutput: 32768},
	{ID: "stealth/glyph-cluster", Context: 256000, MaxOutput: 256000},
	{ID: "openrouter/free", Context: 200000, MaxOutput: 0},
}

// fallbackModels is the cold-start catalogue: the curated table plus whatever
// the operator added through extra_models.
func fallbackModels(cfg Config) []core.Model {
	out := make([]core.Model, 0, len(curatedModels)+len(cfg.ExtraModels))
	seen := make(map[string]bool, len(curatedModels)+len(cfg.ExtraModels))
	for _, m := range curatedModels {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		model := newModel(m.ID)
		if m.Context > 0 {
			model.Extra["context_length"] = m.Context
		}
		if m.MaxOutput > 0 {
			model.Extra["max_output_tokens"] = m.MaxOutput
		}
		model.Extra["source"] = "builtin"
		model.Extra["free"] = true
		out = append(out, model)
	}
	for _, id := range cfg.ExtraModels {
		if seen[id] {
			continue
		}
		seen[id] = true
		model := newModel(id)
		model.Extra["source"] = "config"
		if looksFreeByID(id) {
			model.Extra["free"] = true
		}
		out = append(out, model)
	}
	return restrictToFree(out, cfg)
}

// restrictToFree keeps only the zero-cost entries when free_only is on.  The
// cold-start catalogue is a mixed list (curated ids plus extra_models), and a
// free-only deployment must neither advertise nor call a paid id from it.
func restrictToFree(list []core.Model, cfg Config) []core.Model {
	if !cfg.freeOnly() {
		return list
	}
	out := make([]core.Model, 0, len(list))
	for _, m := range list {
		if modelIsFree(m) {
			out = append(out, m)
		}
	}
	return out
}

// newModel builds the bare shape every catalogue entry shares.
func newModel(id string) core.Model {
	return core.Model{
		ID:      id,
		OwnedBy: authorOf(id),
		Extra:   map[string]any{"vendor": clientName},
	}
}

// authorOf is the vendor prefix of an id.  Gateway ids are `author/slug` with
// an optional `:variant` suffix (and `kilo-auto/*` routers).
func authorOf(id string) string {
	id = strings.TrimSpace(id)
	if i := strings.Index(id, ":"); i >= 0 {
		id = id[:i]
	}
	if i := strings.Index(id, "/"); i > 0 {
		return id[:i]
	}
	return ""
}

// --- wire decoding --------------------------------------------------------

// parsePrice turns one pricing field into a number.  A missing, empty or
// unparseable value is "unknown", which is deliberately different from zero: a
// vendor that omits pricing must not have its model advertised as free.
func parsePrice(p priceString) (float64, bool) {
	s := strings.TrimSpace(string(p))
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// isFreePricing reports whether both directions are published AND zero: a
// model that is free to prompt but charged per completion token is not a free
// model, and one that publishes nothing is unknown rather than free.
func isFreePricing(prompt, completion priceString) bool {
	p, ok := parsePrice(prompt)
	if !ok {
		return false
	}
	c, ok := parsePrice(completion)
	if !ok {
		return false
	}
	return p == 0 && c == 0
}

// looksFreeByID is the cold-start heuristic: the vendor names many of its
// zero-cost variants with a ":free" suffix, and "kilo-auto/free" is the
// router's own free alias.  It is only consulted before a live catalogue
// exists, because the live pricing and isFree flag are authoritative.
func looksFreeByID(id string) bool {
	id = strings.TrimSpace(id)
	return strings.HasSuffix(id, ":free") || id == "kilo-auto/free" || id == "openrouter/free"
}

// modelIsFree reads the catalogue's own free marker.
func modelIsFree(m core.Model) bool {
	if m.Extra == nil {
		return false
	}
	v, _ := m.Extra["free"].(bool)
	return v
}

// freeModelSet is the set of ids this deployment may call, derived from the
// served catalogue so it tracks live pricing rather than a fixed list.
func (c *Client) freeModelSet() map[string]bool {
	list := c.statusModels()
	out := make(map[string]bool, len(list))
	for _, m := range list {
		if modelIsFree(m) {
			out[strings.TrimSpace(m.ID)] = true
		}
	}
	return out
}

// modelAllowed reports whether free_only admits this id.  The catalogue is
// narrowed too, but an explicitly prefixed paid id must still be refused here
// so it cannot reach the vendor and spend paid credit.
func (c *Client) modelAllowed(id string) bool {
	if !c.cfg.freeOnly() {
		return true
	}
	return c.freeModelSet()[strings.TrimSpace(id)]
}

// priceString decodes a pricing field, which the live response sends as a
// STRING ("0.000003") even though the vendor's schema calls it a number.  Both
// forms are accepted and the raw text is preserved for the panel.
type priceString string

func (p *priceString) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*p = ""
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*p = priceString(v)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*p = priceString(n.String())
	return nil
}

type kiloModelWire struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Created      int64  `json:"created"`
	Description  string `json:"description"`
	Architecture struct {
		InputModalities  []string `json:"input_modalities"`
		OutputModalities []string `json:"output_modalities"`
		Tokenizer        string   `json:"tokenizer"`
	} `json:"architecture"`
	TopProvider struct {
		IsModerated         bool `json:"is_moderated"`
		ContextLength       int  `json:"context_length"`
		MaxCompletionTokens int  `json:"max_completion_tokens"`
	} `json:"top_provider"`
	Pricing struct {
		Prompt     priceString `json:"prompt"`
		Completion priceString `json:"completion"`
	} `json:"pricing"`
	ContextLength         int      `json:"context_length"`
	SupportedParameters   []string `json:"supported_parameters"`
	IsFree                bool     `json:"isFree"`
	MayTrainOnYourPrompts bool     `json:"mayTrainOnYourPrompts"`
	AutoRouting           struct {
		Models []string `json:"models"`
	} `json:"autoRouting"`
}

// parseModelsBody decodes `{"data":[…]}`.  A model with no id is skipped rather
// than surfaced as a nameless row.
func parseModelsBody(body []byte) ([]core.Model, error) {
	var env struct {
		Data []kiloModelWire `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("%s: model list: %w", clientName, err)
	}
	out := make([]core.Model, 0, len(env.Data))
	seen := make(map[string]bool, len(env.Data))
	for _, m := range env.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, modelFromWire(m))
	}
	return out, nil
}

func modelFromWire(m kiloModelWire) core.Model {
	id := strings.TrimSpace(m.ID)
	model := newModel(id)
	model.Extra["source"] = "live"
	if name := strings.TrimSpace(m.Name); name != "" {
		model.Extra["name"] = name
	}
	if d := strings.TrimSpace(m.Description); d != "" {
		model.Extra["description"] = d
	}
	ctxLen := m.ContextLength
	if m.TopProvider.ContextLength > 0 {
		ctxLen = m.TopProvider.ContextLength
		model.Extra["provider_context_length"] = m.TopProvider.ContextLength
	}
	if ctxLen > 0 {
		model.Extra["context_length"] = ctxLen
	}
	// top_provider.max_completion_tokens is the real per-model output budget;
	// the router pseudo-models can omit it, and then we publish nothing rather
	// than guessing a number that would make the gateway over-ask.
	if m.TopProvider.MaxCompletionTokens > 0 {
		model.Extra["max_output_tokens"] = m.TopProvider.MaxCompletionTokens
	}
	model.Extra["is_moderated"] = m.TopProvider.IsModerated
	if len(m.Architecture.InputModalities) > 0 {
		model.Extra["input_modalities"] = append([]string(nil), m.Architecture.InputModalities...)
	}
	if len(m.Architecture.OutputModalities) > 0 {
		model.Extra["output_modalities"] = append([]string(nil), m.Architecture.OutputModalities...)
	}
	if m.Pricing.Prompt != "" {
		model.Extra["pricing_prompt"] = string(m.Pricing.Prompt)
	}
	if m.Pricing.Completion != "" {
		model.Extra["pricing_completion"] = string(m.Pricing.Completion)
	}
	// Freeness is the union of three independent signals: the vendor's own
	// isFree flag, the ":free" id convention, and published-zero pricing.  A
	// name-based rule would wrongly mark a metered model free.
	if m.IsFree || looksFreeByID(id) || isFreePricing(m.Pricing.Prompt, m.Pricing.Completion) {
		model.Extra["free"] = true
	}
	if m.MayTrainOnYourPrompts {
		model.Extra["may_train_on_prompts"] = true
	}
	if len(m.SupportedParameters) > 0 {
		model.Extra["supported_parameters"] = append([]string(nil), m.SupportedParameters...)
	}
	if len(m.AutoRouting.Models) > 0 {
		model.Extra["auto_routing"] = append([]string(nil), m.AutoRouting.Models...)
	}
	if m.Created > 0 {
		model.Extra["created"] = m.Created
	}
	return model
}

// --- cache ----------------------------------------------------------------

// modelCache holds the last live catalogue.  It is the only place that decides
// what "fresh" means, so a test can pin the TTL behaviour without a network.
type modelCache struct {
	mu          sync.Mutex
	list        []core.Model
	fetchedAt   time.Time
	attemptedAt time.Time
	loading     bool
}

func (mc *modelCache) snapshot() []core.Model {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	return cloneModels(mc.list)
}

// fresh reports whether the cached list may still be served without a fetch.
func (mc *modelCache) fresh(now time.Time, ttl time.Duration) ([]core.Model, bool) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if len(mc.list) == 0 || ttl <= 0 {
		return nil, false
	}
	if now.Sub(mc.fetchedAt) >= ttl {
		return nil, false
	}
	return cloneModels(mc.list), true
}

// beginRefresh is the single-flight gate.  It refuses when a refresh is already
// running and when the previous attempt is inside the throttle, so a vendor
// outage cannot turn into a request storm.
func (mc *modelCache) beginRefresh(now time.Time, throttle time.Duration) bool {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if mc.loading {
		return false
	}
	if !mc.attemptedAt.IsZero() && throttle > 0 && now.Sub(mc.attemptedAt) < throttle {
		return false
	}
	mc.loading = true
	mc.attemptedAt = now
	return true
}

func (mc *modelCache) endRefresh() {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.loading = false
}

func (mc *modelCache) store(list []core.Model, at time.Time) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.list = cloneModels(list)
	mc.fetchedAt = at
}

func cloneModels(in []core.Model) []core.Model {
	if len(in) == 0 {
		return nil
	}
	out := make([]core.Model, 0, len(in))
	for _, m := range in {
		cp := m
		if m.Extra != nil {
			extra := make(map[string]any, len(m.Extra))
			for k, v := range m.Extra {
				extra[k] = v
			}
			cp.Extra = extra
		}
		out = append(out, cp)
	}
	return out
}

// --- catalogue ------------------------------------------------------------

// lastModels is the last list the network gave us, at any age.
func (c *Client) lastModels() []core.Model { return c.models.snapshot() }

// statusModels is what Status() and a credential-less Models() serve.  It never
// touches the network.
func (c *Client) statusModels() []core.Model {
	if list := c.lastModels(); len(list) > 0 {
		return list
	}
	return fallbackModels(c.cfg)
}

// freeFallbackModel is the id the probe falls back to when free_only is on and
// the catalogue has no free row to offer: the gateway's own zero-cost alias.
const freeFallbackModel = "kilo-auto/free"

// probeModel is the id the panel's Test button probes with.  An explicit
// test_model wins, except under free_only where a paid id is not a valid probe:
// the policy is a promise not to spend paid credit.  Otherwise the router's
// free alias is preferred, then the first free model the catalogue lists.
func (c *Client) probeModel() string {
	configured := strings.TrimSpace(c.cfg.TestModel)
	if configured != "" && (!c.cfg.freeOnly() || c.modelAllowed(configured)) {
		return configured
	}
	if !c.cfg.freeOnly() || c.modelAllowed(freeFallbackModel) {
		return freeFallbackModel
	}
	for _, m := range c.statusModels() {
		if modelIsFree(m) {
			return m.ID
		}
	}
	if c.cfg.freeOnly() {
		return freeFallbackModel
	}
	return c.cfg.testModel()
}

// modelIDs is the bare id list the panel renders.
func (c *Client) modelIDs() []string {
	list := c.statusModels()
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.ID)
	}
	return out
}

// Models never blocks and never fails: the registry calls it on every module
// just to resolve a bare model name, so a slow vendor must not slow the gateway
// down.  A cold cache kicks off a background refresh and answers from the
// built-in catalogue in the meantime.
func (c *Client) Models(ctx context.Context) ([]core.Model, error) {
	c.ensure()
	now := c.now()
	if list, ok := c.models.fresh(now, c.cfg.modelsTTL()); ok {
		return list, nil
	}
	// With no usable credential at all the contract forbids making a request:
	// serve the configured/built-in list, do not count an attempt, do not
	// error.  (The anonymous identity means this is normally not the case.)
	if c.pool.enabled() == 0 {
		return c.statusModels(), nil
	}
	c.refreshModelsAsync()
	if list := c.lastModels(); len(list) > 0 {
		return list, nil
	}
	return c.statusModels(), nil
}

// RefreshModels is the synchronous path (the panel's refresh button).  It
// bypasses the TTL, and a failure never empties the catalogue: the last
// known-good list is returned ALONGSIDE the error so the panel can mark the
// rows as fallback instead of showing an empty picker.
func (c *Client) RefreshModels(ctx context.Context) ([]core.Model, error) {
	c.ensure()
	if c.pool.enabled() == 0 {
		return c.statusModels(), fmt.Errorf("%w: no Kilo credential to refresh models with", core.ErrNotConfigured)
	}
	list, err := c.fetchModels(ctx)
	if err != nil {
		if last := c.lastModels(); len(last) > 0 {
			return last, err
		}
		return c.statusModels(), err
	}
	return list, nil
}

// refreshModelsAsync starts a background catalogue refresh, guarded so a panic
// cannot take the process down.
func (c *Client) refreshModelsAsync() {
	if !c.models.beginRefresh(c.now(), c.cfg.modelsTTL()) {
		return
	}
	core.GoSafe(clientName+" models refresh", func(msg string) { c.noteError(msg) }, func() {
		defer c.models.endRefresh()
		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.modelsTimeout())
		defer cancel()
		if _, err := c.fetchModels(ctx); err != nil {
			c.noteError("model refresh: " + err.Error())
		}
	})
}

// fetchModels performs the live `GET /models` call.  The endpoint is public,
// but a credential is still sent when one exists: the gateway returns a
// per-account catalogue for an authenticated caller.
func (c *Client) fetchModels(ctx context.Context) ([]core.Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	acct, ok := c.pool.firstReady(c.now(), c.limit())
	if !ok {
		return nil, fmt.Errorf("%w: no Kilo credential", core.ErrNotConfigured)
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.modelsTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.modelsURL(), nil)
	if err != nil {
		return nil, err
	}
	c.cfg.applyHeaders(req, acct.bearer(), "")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, c.classifyUpstream(acct.ID, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBytes))
	if err != nil {
		return nil, c.classifyUpstream(acct.ID, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.classifyHTTP("models", acct.ID, resp.StatusCode, body)
	}
	list, err := parseModelsBody(body)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("%s: the vendor returned an empty model catalogue", clientName)
	}
	// free_only narrows what is cached, so every later read (the model list,
	// the panel picker, the chat guard's allowed set) already agrees.
	list = restrictToFree(list, c.cfg)
	c.models.store(list, c.now())
	c.noteSuccess(acct)
	return list, nil
}

// ModelMaxOutputTokens answers from the in-memory catalogue ONLY.
//
// It runs inside a chat request that omitted max_tokens, so it must never
// fetch: a model the catalogue does not know is answered with ok=false, which
// is a normal answer, not a failure.  The catalogue is the live list once one
// has been fetched, and the recorded built-in table before that.
func (c *Client) ModelMaxOutputTokens(ctx context.Context, model string) (int, bool) {
	c.ensure()
	return core.OutputLimitFor(c.statusModels(), model)
}
