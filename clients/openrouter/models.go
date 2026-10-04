package openrouter

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

// maxModelsBytes bounds the catalogue read.  The live response is ~760 KB; the
// cap exists so a broken or hostile endpoint cannot exhaust memory.
const maxModelsBytes = 32 << 20

// curatedModel is one row of the built-in catalogue.
//
// Context and MaxOutput are the vendor's own published numbers, recorded from a
// live `GET /models` probe (see README "Verified vs assumed").  They are the
// cold-start answer: a fresh process can describe the common models and answer
// ModelMaxOutputTokens before it has ever spoken to the network.
type curatedModel struct {
	ID        string
	Context   int
	MaxOutput int
}

// curatedModels is a deliberately small slice of the vendor's 462 live ids:
// enough that a cold cache serves something useful, not a 760 KB Go literal.
// The live `GET /models` response always wins when it is available.
var curatedModels = []curatedModel{
	{ID: "openai/gpt-6.1-sol-pro", Context: 1050000, MaxOutput: 128000},
	{ID: "openai/gpt-6-luna", Context: 1050000, MaxOutput: 128000},
	{ID: "openai/gpt-6-sol", Context: 1050000, MaxOutput: 128000},
	{ID: "openai/gpt-5.5", Context: 1050000, MaxOutput: 128000},
	{ID: "openai/gpt-5.2", Context: 400000, MaxOutput: 128000},
	{ID: "openai/gpt-5.1", Context: 400000, MaxOutput: 128000},
	{ID: "openai/gpt-5", Context: 400000, MaxOutput: 128000},
	{ID: "openai/gpt-5-mini", Context: 400000, MaxOutput: 128000},
	{ID: "openai/gpt-5-nano", Context: 400000, MaxOutput: 128000},
	{ID: "openai/gpt-oss-120b", Context: 131072, MaxOutput: 117964},
	{ID: "openai/o3", Context: 200000, MaxOutput: 100000},
	{ID: "openai/o4-mini", Context: 200000, MaxOutput: 100000},
	{ID: "openai/gpt-4.1", Context: 1047576, MaxOutput: 32768},
	{ID: "openai/gpt-4o", Context: 128000, MaxOutput: 16384},
	{ID: "openai/gpt-4o-mini", Context: 128000, MaxOutput: 16384},
	{ID: "openai/gpt-3.5-turbo", Context: 16385, MaxOutput: 4096},
	{ID: "anthropic/claude-sonnet-5.5", Context: 1000000, MaxOutput: 128000},
	{ID: "anthropic/claude-opus-5.5", Context: 1000000, MaxOutput: 128000},
	{ID: "anthropic/claude-opus-5", Context: 1000000, MaxOutput: 128000},
	{ID: "anthropic/claude-sonnet-5", Context: 1000000, MaxOutput: 128000},
	{ID: "anthropic/claude-fable-5", Context: 1000000, MaxOutput: 128000},
	{ID: "anthropic/claude-opus-4.8", Context: 1000000, MaxOutput: 128000},
	{ID: "anthropic/claude-sonnet-4.6", Context: 1000000, MaxOutput: 128000},
	{ID: "anthropic/claude-opus-4.5", Context: 200000, MaxOutput: 64000},
	{ID: "anthropic/claude-haiku-4.5", Context: 200000, MaxOutput: 64000},
	{ID: "anthropic/claude-sonnet-4.5", Context: 1000000, MaxOutput: 64000},
	{ID: "google/gemini-3.8-flash", Context: 1048576, MaxOutput: 65536},
	{ID: "google/gemini-3.7-flash", Context: 1048576, MaxOutput: 65536},
	{ID: "google/gemini-3.5-flash", Context: 1048576, MaxOutput: 65536},
	{ID: "google/gemini-3.1-pro-preview", Context: 1048576, MaxOutput: 65536},
	{ID: "google/gemini-3-flash-preview", Context: 1048576, MaxOutput: 65536},
	{ID: "google/gemini-2.5-pro", Context: 1048576, MaxOutput: 65536},
	{ID: "google/gemini-2.5-flash", Context: 1048576, MaxOutput: 65535},
	{ID: "google/gemini-2.5-flash-lite", Context: 1048576, MaxOutput: 65535},
	{ID: "google/gemma-4-31b-it", Context: 262144, MaxOutput: 16384},
	{ID: "google/gemma-3-27b-it", Context: 131072, MaxOutput: 117964},
	{ID: "deepseek/deepseek-v4.1-flash", Context: 1048576, MaxOutput: 943718},
	{ID: "deepseek/deepseek-v4-pro", Context: 1048576, MaxOutput: 384000},
	{ID: "deepseek/deepseek-v4-flash", Context: 1048576, MaxOutput: 131072},
	{ID: "deepseek/deepseek-v3.2", Context: 163840, MaxOutput: 65536},
	{ID: "deepseek/deepseek-r1", Context: 64000, MaxOutput: 16000},
	{ID: "qwen/qwen3.8-max-prime", Context: 1000000, MaxOutput: 131072},
	{ID: "qwen/qwen3.8-flash", Context: 1000000, MaxOutput: 131072},
	{ID: "qwen/qwen3.7-max", Context: 1000000, MaxOutput: 131072},
	{ID: "qwen/qwen3-coder-plus", Context: 1000000, MaxOutput: 65536},
	{ID: "qwen/qwen3-coder", Context: 262144, MaxOutput: 65536},
	{ID: "qwen/qwen3-235b-a22b-2507", Context: 262144, MaxOutput: 235929},
	{ID: "z-ai/glm-5.3", Context: 1048576, MaxOutput: 943718},
	{ID: "z-ai/glm-5.2", Context: 1048576, MaxOutput: 943718},
	{ID: "z-ai/glm-5", Context: 204800, MaxOutput: 128000},
	{ID: "z-ai/glm-4.7", Context: 204800, MaxOutput: 131072},
	{ID: "z-ai/glm-4.5", Context: 131072, MaxOutput: 98304},
	{ID: "moonshotai/kimi-k3", Context: 1048576, MaxOutput: 943718},
	{ID: "moonshotai/kimi-k2.6", Context: 262144, MaxOutput: 235929},
	{ID: "moonshotai/kimi-k2.5", Context: 262144, MaxOutput: 235929},
	{ID: "moonshotai/kimi-k2-thinking", Context: 262144, MaxOutput: 235929},
	{ID: "x-ai/grok-4.7", Context: 500000, MaxOutput: 450000},
	{ID: "x-ai/grok-4.6", Context: 500000, MaxOutput: 450000},
	{ID: "x-ai/grok-4.5", Context: 500000, MaxOutput: 450000},
	{ID: "x-ai/grok-4.3", Context: 1000000, MaxOutput: 900000},
	{ID: "mistralai/mistral-large-2512", Context: 262144, MaxOutput: 209715},
	{ID: "mistralai/mistral-medium-3-5", Context: 262144, MaxOutput: 209715},
	{ID: "mistralai/codestral-2508", Context: 256000, MaxOutput: 204800},
	{ID: "mistralai/devstral-2512", Context: 262144, MaxOutput: 209715},
	{ID: "meta-llama/llama-4-maverick", Context: 1048576, MaxOutput: 16384},
	{ID: "meta-llama/llama-4-scout", Context: 1310720, MaxOutput: 16384},
	{ID: "meta-llama/llama-3.3-70b-instruct", Context: 131072, MaxOutput: 16384},
	{ID: "amazon/nova-pro-v1", Context: 300000, MaxOutput: 5120},
	{ID: "amazon/nova-2-lite-v1", Context: 1000000, MaxOutput: 65535},
	{ID: "amazon/nova-lite-v1", Context: 300000, MaxOutput: 5120},
	{ID: "cohere/command-a-plus", Context: 192000, MaxOutput: 64000},
	{ID: "cohere/command-a", Context: 256000, MaxOutput: 8192},
	{ID: "cohere/north-mini-code:free", Context: 256000, MaxOutput: 64000},
	{ID: "microsoft/phi-4", Context: 16384, MaxOutput: 14745},
	{ID: "nvidia/nemotron-3-super-120b-a12b", Context: 262144, MaxOutput: 235929},
	{ID: "nvidia/nemotron-3-ultra-550b-a55b", Context: 262144, MaxOutput: 182520},
	{ID: "minimax/minimax-m3", Context: 1048576, MaxOutput: 512000},
	{ID: "tencent/hy3", Context: 262144, MaxOutput: 128000},
	{ID: "xiaomi/mimo-v2.6-pro", Context: 1050000, MaxOutput: 131072},
	{ID: "stepfun/step-3.7-flash", Context: 262144, MaxOutput: 230400},
	{ID: "inclusionai/ling-3.0-flash", Context: 262144, MaxOutput: 32768},
	{ID: "sakana/fugu-max", Context: 1000000, MaxOutput: 128000},
	{ID: "upstage/solar-pro4", Context: 524288, MaxOutput: 131072},
	{ID: "aion-labs/aion-3.5", Context: 262144, MaxOutput: 32768},
	{ID: "perplexity/sonar", Context: 127072, MaxOutput: 114364},
	// The two router pseudo-models publish no output budget at all (the live
	// response carries neither top_provider.context_length nor
	// max_completion_tokens for them), so MaxOutput stays 0 = "not published".
	{ID: "openrouter/auto", Context: 2000000, MaxOutput: 0},
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
		if looksFreeByID(m.ID) {
			model.Extra["free"] = true
		}
		out = append(out, model)
	}
	for _, id := range cfg.ExtraModels {
		if seen[id] {
			continue
		}
		seen[id] = true
		model := newModel(id)
		model.Extra["source"] = "config"
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

// authorOf is the vendor prefix of an id.  OpenRouter ids are `author/slug`
// with an optional `:variant` suffix.
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
// unparseable value is "unknown", which is deliberately different from zero:
// a vendor that omits pricing must not have its model advertised as free.
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

// isFreePricing reports whether a model is free to call.  Both directions
// must be published AND zero: a model that is free to prompt but charged per
// completion token is not a free model, and one that publishes nothing is
// unknown rather than free.
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

// looksFreeByID is the cold-start heuristic: the vendor names its zero-cost
// variants with a ":free" suffix, and "openrouter/free" is the router's own
// free alias.  It is only consulted before a live catalogue exists, because
// the live pricing is authoritative.
func looksFreeByID(id string) bool {
	id = strings.TrimSpace(id)
	return strings.HasSuffix(id, ":free") || id == "openrouter/free"
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

type orModelWire struct {
	ID            string `json:"id"`
	CanonicalSlug string `json:"canonical_slug"`
	Name          string `json:"name"`
	Created       int64  `json:"created"`
	ContextLength int    `json:"context_length"`
	Architecture  struct {
		Modality         string   `json:"modality"`
		InputModalities  []string `json:"input_modalities"`
		OutputModalities []string `json:"output_modalities"`
		Tokenizer        string   `json:"tokenizer"`
	} `json:"architecture"`
	Pricing struct {
		Prompt     priceString `json:"prompt"`
		Completion priceString `json:"completion"`
	} `json:"pricing"`
	TopProvider struct {
		ContextLength       int  `json:"context_length"`
		MaxCompletionTokens int  `json:"max_completion_tokens"`
		IsModerated         bool `json:"is_moderated"`
	} `json:"top_provider"`
	SupportedParameters []string `json:"supported_parameters"`
	KnowledgeCutoff     string   `json:"knowledge_cutoff"`
	Reasoning           struct {
		Mandatory bool `json:"mandatory"`
	} `json:"reasoning"`
}

// parseModelsBody decodes `{"data":[…]}`.  A model with no id is skipped rather
// than surfaced as a nameless row.
func parseModelsBody(body []byte) ([]core.Model, error) {
	var env struct {
		Data []orModelWire `json:"data"`
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

func modelFromWire(m orModelWire) core.Model {
	model := newModel(strings.TrimSpace(m.ID))
	model.Extra["source"] = "live"
	if name := strings.TrimSpace(m.Name); name != "" {
		model.Extra["name"] = name
	}
	if m.CanonicalSlug != "" {
		model.Extra["canonical_slug"] = m.CanonicalSlug
	}
	if m.ContextLength > 0 {
		model.Extra["context_length"] = m.ContextLength
	}
	// top_provider.max_completion_tokens is the real per-model output budget;
	// the router pseudo-models omit it, and then we publish nothing rather than
	// guessing a number that would make the gateway over-ask.
	if m.TopProvider.MaxCompletionTokens > 0 {
		model.Extra["max_output_tokens"] = m.TopProvider.MaxCompletionTokens
	}
	if m.TopProvider.ContextLength > 0 {
		model.Extra["provider_context_length"] = m.TopProvider.ContextLength
	}
	model.Extra["is_moderated"] = m.TopProvider.IsModerated
	if m.Architecture.Modality != "" {
		model.Extra["modality"] = m.Architecture.Modality
	}
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
	if isFreePricing(m.Pricing.Prompt, m.Pricing.Completion) {
		model.Extra["free"] = true
	}
	if len(m.SupportedParameters) > 0 {
		model.Extra["supported_parameters"] = append([]string(nil), m.SupportedParameters...)
	}
	if m.KnowledgeCutoff != "" {
		model.Extra["knowledge_cutoff"] = m.KnowledgeCutoff
	}
	if m.Reasoning.Mandatory {
		model.Extra["reasoning_mandatory"] = true
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
// the catalogue has no free row to offer: the router's own zero-cost alias.
const freeFallbackModel = "openrouter/free"

// probeModel is the id the panel's Test button probes with.  An explicit
// test_model wins, except under free_only where a paid id is not a valid
// probe: the policy is a promise not to spend paid credit, and the built-in
// default (openai/gpt-4o-mini) is paid.  Otherwise the first model the
// catalogue marks free is preferred.
func (c *Client) probeModel() string {
	configured := strings.TrimSpace(c.cfg.TestModel)
	if configured != "" && (!c.cfg.freeOnly() || c.modelAllowed(configured)) {
		return configured
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
// just to resolve a bare model name, so a slow vendor must not slow the
// gateway down.  A cold cache kicks off a background refresh and answers from
// the built-in catalogue in the meantime.
func (c *Client) Models(ctx context.Context) ([]core.Model, error) {
	c.ensure()
	now := c.now()
	if list, ok := c.models.fresh(now, c.cfg.modelsTTL()); ok {
		return list, nil
	}
	// With no credential at all the contract forbids making a request: serve
	// the configured/built-in list, do not count an attempt, do not error.
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
		return c.statusModels(), fmt.Errorf("%w: no OpenRouter credential to refresh models with", core.ErrNotConfigured)
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
// but a credential is still sent when one exists: OpenRouter returns a
// per-user catalogue for an authenticated caller.
func (c *Client) fetchModels(ctx context.Context) ([]core.Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	acct, ok := c.pool.firstReady(c.now(), c.limit())
	if !ok {
		return nil, fmt.Errorf("%w: no OpenRouter credential", core.ErrNotConfigured)
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.modelsTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.modelsURL(), nil)
	if err != nil {
		return nil, err
	}
	c.cfg.applyHeaders(req, acct.APIKey)
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
