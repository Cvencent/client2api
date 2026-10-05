package cline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

const (
	// modelsRetryInterval throttles background refreshes so a failing upstream
	// cannot be hammered by every /v1/models call.
	modelsRetryInterval = 30 * time.Second
	// maxModelsBodyBytes bounds a catalogue response body.
	maxModelsBodyBytes = 4 << 20
	// ownedBy is the value every catalogue entry is attributed to.
	ownedBy = "cline"
)

// errNoCatalog is the local answer for "cannot say" on a cold cache.
var errNoCatalog = errors.New("cline: no catalogue available")

// --- built-in fallback catalogue -------------------------------------------

// builtinModel is one entry of the offline catalogue.  maxTokens is the vendor's
// advertised output ceiling, which is NOT uniform: gemini-3.8-flash advertises
// 65 536 while its siblings advertise 131 072, and sending the larger number to
// it is rejected with HTTP 400.
type builtinModel struct {
	ID          string
	Name        string
	Description string
	Context     int
	MaxTokens   int
	Free        bool
}

// builtinModels is used when no live catalogue can be fetched.  It is the ONLY
// source of the free ids when the recommended endpoint is unreachable.
var builtinModels = []builtinModel{
	{
		ID:          "stealth/space-bunny-alpha",
		Name:        "Space Bunny Alpha",
		Description: "Blazing-fast inference with 1M context",
		Context:     1_000_000,
		MaxTokens:   524_288,
		Free:        true,
	},
	{
		ID:          "cline-free/mimo-v2.6-flash",
		Name:        "MiMo-V2.6-Flash",
		Description: "Mixture-of-Experts architecture with 309B total parameters",
		Context:     1_048_576,
		MaxTokens:   131_072,
		Free:        true,
	},
	{
		ID:          "cline-free/deepseek-v4.1-flash",
		Name:        "DeepSeek V4.1 Flash",
		Description: "DeepSeek V4.1 Flash on the free tier",
		Context:     1_048_576,
		MaxTokens:   131_072,
		Free:        true,
	},
	{
		ID:          "cline-free/gemini-3.8-flash",
		Name:        "Gemini 3.8 Flash",
		Description: "Gemini 3.8 Flash on the free tier",
		Context:     1_048_576,
		MaxTokens:   65_536,
		Free:        true,
	},
	{
		ID:          "cline-free/muse-spark-1.3-contributor",
		Name:        "Muse Spark 1.3 Contributor",
		Description: "Muse Spark 1.3 Contributor on the free tier",
		Context:     1_048_576,
		MaxTokens:   943_718,
		Free:        true,
	},
}

// --- wire shapes -----------------------------------------------------------

// modelsEnvelope decodes GET /api/v1/models, which is a plain OpenAI list.
type modelsEnvelope struct {
	Data   []modelEntry `json:"data"`
	Models []modelEntry `json:"models"`
	Object string       `json:"object"`
}

// modelEntry is one entry of either catalogue.  A single shape is used for both
// so a future field appearing on one endpoint does not need a second decoder.
type modelEntry struct {
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	DisplayName      string          `json:"display_name"`
	Description      string          `json:"description"`
	OwnedBy          string          `json:"owned_by"`
	Object           string          `json:"object"`
	Tags             json.RawMessage `json:"tags"`
	ContextLength    any             `json:"context_length"`
	ContextWindow    any             `json:"context_window"`
	MaxOutputTokens  any             `json:"max_output_tokens"`
	MaxTokens        any             `json:"max_tokens"`
	IsFree           bool            `json:"isFree"`
	Free             bool            `json:"free"`
	SupportsImages   *bool           `json:"supportsImages"`
	SupportsVision   *bool           `json:"supports_vision"`
	SupportsThinking *bool           `json:"supportsThinking"`
}

// recommendedEnvelope decodes GET /api/v1/ai/cline/recommended-models.  The
// top-level free array is the ONLY place the free ids come from.
type recommendedEnvelope struct {
	Data        []modelEntry      `json:"data"`
	Models      []modelEntry      `json:"models"`
	Recommended []modelEntry      `json:"recommended"`
	Free        []json.RawMessage `json:"free"`
}

// freeRef is one member of the recommended endpoint's free array, which holds
// either a bare id string or an object.
type freeRef struct {
	ID    string `json:"id"`
	Model string `json:"model"`
}

// ids extracts the ids a free array names.
func (e recommendedEnvelope) ids() []string {
	out := make([]string, 0, len(e.Free))
	for _, raw := range e.Free {
		trimmed := strings.TrimSpace(string(raw))
		if trimmed == "" {
			continue
		}
		if trimmed[0] == '"' {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
			continue
		}
		var ref freeRef
		if json.Unmarshal(raw, &ref) == nil {
			if id := firstNonEmpty(ref.ID, ref.Model); id != "" {
				out = append(out, id)
			}
		}
	}
	return out
}

// entries returns the recommended list under any of its spellings.
func (e recommendedEnvelope) entries() []modelEntry {
	switch {
	case len(e.Data) > 0:
		return e.Data
	case len(e.Models) > 0:
		return e.Models
	default:
		return e.Recommended
	}
}

// --- parsing ---------------------------------------------------------------

// isFreeID is the id-based half of the freeness rule: an id ending in ":free"
// or starting with "cline-free/".  The name is never consulted.
func isFreeID(id string) bool {
	id = strings.TrimSpace(id)
	return strings.HasSuffix(id, ":free") || strings.HasPrefix(id, "cline-free/")
}

// entryFree is the full freeness rule:
//
//	free = (in the remote free array) ∪ (id ends with :free)
//	     ∪ (id starts with cline-free/) ∪ (the entry's own isFree flag)
//
// The model's NAME is deliberately not part of the rule: cline-free/deepseek-v4.1-flash
// is free while deepseek/deepseek-v4.1-flash is metered, and the two are
// different entries with different names only by prefix.
func entryFree(e modelEntry, freeIDs map[string]bool) bool {
	id := strings.TrimSpace(e.ID)
	if id == "" {
		return false
	}
	if freeIDs[id] || isFreeID(id) {
		return true
	}
	return e.IsFree || e.Free
}

// contextOf reads the context window under either spelling.
func contextOf(e modelEntry) int {
	for _, v := range []any{e.ContextLength, e.ContextWindow} {
		if n, ok := asInt(v); ok && n > 0 {
			return n
		}
	}
	return 0
}

// maxTokensOf reads the advertised output ceiling under either spelling.
func maxTokensOf(e modelEntry) int {
	for _, v := range []any{e.MaxOutputTokens, e.MaxTokens} {
		if n, ok := asInt(v); ok && n > 0 {
			return n
		}
	}
	return 0
}

// tagList decodes the tags member, which may be an array or a bare string.
func tagList(raw json.RawMessage) []string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var single string
	if json.Unmarshal(raw, &single) == nil && strings.TrimSpace(single) != "" {
		return []string{single}
	}
	return nil
}

// supportsImages reports whether the entry advertises image input.
func supportsImages(e modelEntry) bool {
	if e.SupportsImages != nil {
		return *e.SupportsImages
	}
	if e.SupportsVision != nil {
		return *e.SupportsVision
	}
	return false
}

// modelFromEntry projects a wire entry into core.Model.  The max_output_tokens
// Extra key is the repo-wide convention and is what ModelMaxOutputTokens reads.
func modelFromEntry(e modelEntry, freeIDs map[string]bool) (core.Model, bool) {
	id := strings.TrimSpace(e.ID)
	if id == "" {
		return core.Model{}, false
	}
	free := entryFree(e, freeIDs)
	ctxLen := contextOf(e)
	maxTok := maxTokensOf(e)
	name := firstNonEmpty(e.Name, e.DisplayName)
	if ctxLen <= 0 {
		ctxLen = defaultContextLength
	}
	if maxTok <= 0 {
		// A free model's ceiling is its context window; a metered model's is
		// this module's own default.  Guessing high would earn an upstream 400.
		if free {
			maxTok = ctxLen
		} else {
			maxTok = defaultMaxTokens
		}
	}
	extra := map[string]any{
		"context_length":    ctxLen,
		"max_output_tokens": maxTok,
		"free":              free,
	}
	if name != "" {
		extra["display_name"] = name
	}
	if d := strings.TrimSpace(e.Description); d != "" {
		extra["description"] = d
	}
	if tags := tagList(e.Tags); len(tags) > 0 {
		extra["tags"] = tags
	}
	if supportsImages(e) {
		extra["vision"] = true
	}
	if e.SupportsThinking != nil {
		extra["reasoning"] = *e.SupportsThinking
	} else if free {
		// Every free model measured supports thinking.
		extra["reasoning"] = true
	}
	return core.Model{ID: id, OwnedBy: ownedBy, Extra: extra}, true
}

// parseModelsList decodes GET /api/v1/models.  A body with neither a data nor a
// models array is a failure rather than an empty success.
func parseModelsList(raw []byte) ([]core.Model, error) {
	var env modelsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("cline: unreadable model list: %w", err)
	}
	entries := env.Data
	if len(entries) == 0 {
		entries = env.Models
	}
	if len(entries) == 0 {
		return nil, errNoCatalog
	}
	out := make([]core.Model, 0, len(entries))
	for _, e := range entries {
		if m, ok := modelFromEntry(e, nil); ok {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil, errNoCatalog
	}
	return out, nil
}

// parseRecommended decodes the recommended-models response, returning the
// entries and the ids named by the free array.
func parseRecommended(raw []byte) ([]modelEntry, map[string]bool, error) {
	var env recommendedEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, nil, fmt.Errorf("cline: unreadable recommended models: %w", err)
	}
	freeIDs := map[string]bool{}
	for _, id := range env.ids() {
		freeIDs[id] = true
	}
	return env.entries(), freeIDs, nil
}

// mergeCatalog is the three-source merge.  Precedence, lowest first:
//
//  1. the built-in fallback table;
//  2. GET /api/v1/models (the ~460 metered ids);
//  3. GET /api/v1/ai/cline/recommended-models, which is authoritative for
//     metadata AND is the only source of the free ids -- /api/v1/models never
//     lists a cline-free/* id at all.
//
// A model present in more than one source keeps the highest-precedence
// metadata, and the free set is the union across every source.
//
// The built-in table is a fallback for a total outage, not a permanent
// skeleton.  When the recommended endpoint answers, its free array is the
// complete truth for free ids: a built-in free entry it no longer lists is
// dropped, so a model retired upstream disappears on the next refresh instead
// of being resurrected as a permanent 404 for Auto/ routing.  If that endpoint
// failed, the built-in free entries are kept so a partial outage does not make
// free models flap.
func mergeCatalog(builtin []builtinModel, listed []core.Model, recommended []modelEntry, freeIDs map[string]bool, freeAuthoritative bool) []core.Model {
	order := make([]string, 0, len(builtin)+len(listed)+len(recommended))
	byID := map[string]core.Model{}
	free := map[string]bool{}
	for id := range freeIDs {
		free[id] = true
	}

	// liveIDs is the set of ids at least one live source knows about.  The
	// built-in table is filtered against it only when freeAuthoritative is set --
	// that flag means the recommended endpoint answered, and its free array is
	// the complete truth for free ids.  If that endpoint failed we keep the
	// built-in free entries rather than making free models flap during a partial
	// outage.
	liveIDs := make(map[string]bool, len(listed)+len(recommended)+len(freeIDs))
	for _, m := range listed {
		liveIDs[m.ID] = true
	}
	for _, e := range recommended {
		if id := strings.TrimSpace(e.ID); id != "" {
			liveIDs[id] = true
		}
	}
	for id := range freeIDs {
		liveIDs[id] = true
	}
	put := func(id string, m core.Model, isFree bool) {
		if _, seen := byID[id]; !seen {
			order = append(order, id)
		}
		byID[id] = m
		if isFree {
			free[id] = true
		}
	}

	for _, b := range builtin {
		if freeAuthoritative && len(liveIDs) > 0 && !liveIDs[b.ID] {
			continue
		}
		m, ok := modelFromEntry(modelEntry{
			ID:            b.ID,
			Name:          b.Name,
			Description:   b.Description,
			ContextLength: b.Context,
			MaxTokens:     b.MaxTokens,
			IsFree:        b.Free,
		}, free)
		if !ok {
			continue
		}
		m.Extra["fallback"] = true
		put(m.ID, m, b.Free)
	}

	for _, m := range listed {
		// /api/v1/models carries no freeness signal of its own; the id rules
		// and the remote free array still apply.  The flag is written
		// unconditionally so a lower-precedence guess cannot survive here.
		isFree := isFreeID(m.ID) || free[m.ID]
		if m.Extra == nil {
			m.Extra = map[string]any{}
		}
		m.Extra["free"] = isFree
		put(m.ID, m, isFree)
	}

	for _, e := range recommended {
		m, ok := modelFromEntry(e, free)
		if !ok {
			continue
		}
		put(m.ID, m, entryFree(e, free))
	}

	out := make([]core.Model, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out
}

// fallbackCatalog renders the built-in table alone.
func fallbackCatalog() []core.Model {
	return mergeCatalog(builtinModels, nil, nil, nil, false)
}

// modelIDs lists the ids in catalogue order.
func modelIDs(models []core.Model) []string {
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids
}

// --- cache -----------------------------------------------------------------

// modelCache is the single-flight catalogue cache.  It exists so a chat request
// can ask "what does this model advertise?" without ever touching the network.
type modelCache struct {
	mu          sync.Mutex
	list        []core.Model
	storedAt    time.Time
	attemptedAt time.Time
	loading     bool
}

// cached returns the list when it is fresh.
func (mc *modelCache) cached(now time.Time, ttl time.Duration) ([]core.Model, bool) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if len(mc.list) == 0 || mc.storedAt.IsZero() {
		return nil, false
	}
	if ttl > 0 && now.Sub(mc.storedAt) > ttl {
		return nil, false
	}
	return cloneModels(mc.list), true
}

// snapshot returns the list regardless of age, or nil when cold.
func (mc *modelCache) snapshot() []core.Model {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	return cloneModels(mc.list)
}

// triedRecently reports whether a refresh was attempted inside the throttle.
func (mc *modelCache) triedRecently(now time.Time, throttle time.Duration) bool {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if mc.attemptedAt.IsZero() {
		return false
	}
	return now.Sub(mc.attemptedAt) < throttle
}

// beginRefresh claims the single-flight slot.  A false return means a refresh is
// already running, or one was attempted too recently.
func (mc *modelCache) beginRefresh(now time.Time, throttle time.Duration) bool {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if mc.loading {
		return false
	}
	if !mc.attemptedAt.IsZero() && now.Sub(mc.attemptedAt) < throttle {
		return false
	}
	mc.loading = true
	mc.attemptedAt = now
	return true
}

// finishRefresh stores the outcome.  A failed refresh keeps the last good list.
func (mc *modelCache) finishRefresh(list []core.Model, err error, at time.Time) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.loading = false
	if err != nil || len(list) == 0 {
		return
	}
	mc.list = cloneModels(list)
	mc.storedAt = at
}

// cloneModels deep-copies the Extra maps so a caller cannot mutate the cache.
func cloneModels(list []core.Model) []core.Model {
	if len(list) == 0 {
		return nil
	}
	out := make([]core.Model, len(list))
	for i, m := range list {
		cp := m
		if m.Extra != nil {
			extra := make(map[string]any, len(m.Extra))
			for k, v := range m.Extra {
				extra[k] = v
			}
			cp.Extra = extra
		}
		out[i] = cp
	}
	return out
}

// --- client-facing catalogue API -------------------------------------------

// Models answers from the cache, or from the built-in table while a background
// refresh runs.  It never blocks: the registry calls Models on every module for
// a bare model name.
func (c *Client) Models(ctx context.Context) ([]core.Model, error) {
	now := time.Now()
	if list, ok := c.models.cached(now, c.cfg.modelsTTL()); ok {
		return list, nil
	}
	c.refreshModelsAsync()
	if list := c.models.snapshot(); len(list) > 0 {
		return list, nil
	}
	return fallbackCatalog(), nil
}

// RefreshModels performs a synchronous refresh, returning the last good list (or
// the built-in table) alongside any error.
func (c *Client) RefreshModels(ctx context.Context) ([]core.Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	list, err := c.fetchModels(ctx)
	if err == nil {
		c.models.finishRefresh(list, nil, time.Now())
		return list, nil
	}
	if cached := c.models.snapshot(); len(cached) > 0 {
		return cached, err
	}
	return fallbackCatalog(), err
}

// refreshModelsAsync starts one background refresh at a time, throttled by
// modelsRetryInterval.
func (c *Client) refreshModelsAsync() {
	if !c.models.beginRefresh(time.Now(), modelsRetryInterval) {
		return
	}
	core.GoSafe("cline models refresh", func(msg string) { c.deps.Log("cline: %s", msg) }, func() {
		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.modelsTimeout())
		defer cancel()
		list, err := c.fetchModels(ctx)
		if err != nil {
			c.deps.Log("cline: model refresh failed: %v", scrubError(err))
		}
		c.models.finishRefresh(list, err, time.Now())
	})
}

// fetchModels builds the catalogue from the three sources.
func (c *Client) fetchModels(ctx context.Context) ([]core.Model, error) {
	listed, listErr := c.fetchModelsList(ctx)
	recommended, freeIDs, recErr := c.fetchRecommended(ctx)
	if listErr != nil && recErr != nil {
		return nil, fmt.Errorf("cline: no catalogue source answered: %v; %v", scrubError(listErr), scrubError(recErr))
	}
	merged := mergeCatalog(builtinModels, listed, recommended, freeIDs, recErr == nil)
	if len(merged) == 0 {
		return nil, errNoCatalog
	}
	return merged, nil
}

// fetchModelsList asks for the metered catalogue.
func (c *Client) fetchModelsList(ctx context.Context) ([]core.Model, error) {
	status, _, raw, err := c.doJSON(ctx, http.MethodGet, c.cfg.endpoint(modelsPath), c.bearer(), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, errors.New("cline: model list refused: " + errorMessage(string(raw)))
	}
	return parseModelsList(raw)
}

// fetchRecommended asks for the recommended models, which is the only source of
// the free ids.  The endpoint needs NO authentication, so it is called without
// a bearer even when one exists -- that is also what makes the fallback work
// with no credential at all.
func (c *Client) fetchRecommended(ctx context.Context) ([]modelEntry, map[string]bool, error) {
	status, _, raw, err := c.doJSON(ctx, http.MethodGet, c.cfg.endpoint(recommendedPath), "", nil)
	if err != nil {
		return nil, nil, err
	}
	if status != http.StatusOK {
		return nil, nil, errors.New("cline: recommended models refused: " + errorMessage(string(raw)))
	}
	return parseRecommended(raw)
}

// bearer is the Authorization value for a catalogue request: the first usable
// account's token, workos: prefix intact, or "" when there is none.
func (c *Client) bearer() string {
	if c.pool == nil {
		return ""
	}
	if list := c.pool.usable(); len(list) > 0 {
		return list[0].acct.AccessToken
	}
	return ""
}

// ModelMaxOutputTokens implements core.ModelLimitsProvider.
//
// It answers from the cache ONLY.  It runs inside a chat request, so it must
// never fetch: a cold cache means "cannot say" for this one request rather than
// a metadata round trip in the middle of a turn.  Models() refreshes the cache
// in the background, so the answer is usually there.
func (c *Client) ModelMaxOutputTokens(ctx context.Context, model string) (int, bool) {
	return core.OutputLimitFor(c.models.snapshot(), model)
}

// sortedIDs is a stable, sorted copy used by Status.
func sortedIDs(models []core.Model) []string {
	ids := modelIDs(models)
	sort.Strings(ids)
	return ids
}
