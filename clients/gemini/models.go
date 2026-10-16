package gemini

// models.go keeps the module's model catalogue live.
//
// The vendor's Cloud Code Assist line serves a real model list.  Before this
// file the catalogue was one compiled-in id with four effort suffixes, so a
// model Google added (or retired) needed a code change and the panel could not
// tell.  The list now comes from `POST /v1internal:fetchAvailableModels`,
// falling back to the model ids named by `POST /v1internal:retrieveUserQuota`
// when that call is refused.
//
// Three rules shape the design:
//
//   - Models() is called on every panel poll and on every bare-model lookup,
//     so it answers from a TTL cache and never makes the caller wait on the
//     network.  A cold cache starts one background refresh, which the panel's
//     Status poll kicks too, so the live list is up within seconds of startup.
//   - A failed refresh never empties the picker.  RefreshModels returns the
//     last good list (or the compiled-in flash row) alongside the error.
//   - The wire ids stay honest: an effort family (`gemini-x-flash-low` and
//     friends) is advertised as one `gemini-x-flash` row, but each request
//     still sends the exact variant the caller's effort names.  A `-tiered`
//     id is its own row and is forwarded verbatim.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"client2api/internal/core"
)

const (
	// fetchAvailableModelsPath is the vendor's live catalogue.
	fetchAvailableModelsPath = "/v1internal:fetchAvailableModels"
	// retrieveUserQuotaPath names the model ids an account has an allowance
	// of.  It is the fallback when the catalogue call is refused.
	retrieveUserQuotaPath = "/v1internal:retrieveUserQuota"

	// modelsCacheTTL is how long a fetched list is trusted, and how long a
	// failed refresh suppresses the next background retry.  The vendor's
	// catalogue changes on the order of weeks while Models() runs on every
	// panel poll; the panel's refresh button bypasses it entirely.
	modelsCacheTTL = 5 * time.Minute

	// modelsSourceUpstream / modelsSourceFallback mark where a row came from,
	// so the panel can show a compiled-in row as such.
	modelsSourceUpstream = "upstream"
	modelsSourceFallback = "fallback"
	// modelsDiscoveryQuota marks rows that came from the quota fallback.
	modelsDiscoveryQuota = "quota"
)

// effortVariantOrder is the foldable ladder, lowest first.  "-tiered" is
// deliberately absent: the vendor serves it as its own model whose thinking
// level follows the request, not as a sibling of low/medium/high.
var effortVariantOrder = []string{"low", "medium", "high"}

// modelRoute is how one advertised catalogue id reaches the wire.
type modelRoute struct {
	// exact is the admission key for a single-id model.  It is empty for a
	// folded effort family.
	exact string
	// efforts maps an effort label onto the exact upstream id.  It is nil for
	// a single-id model.
	efforts map[string]string
	// defaultID is sent when the caller names no usable effort.  defaultTier
	// is the thinking level that goes with it; an empty defaultTier omits the
	// thinking budget and lets the vendor answer with its own default.
	defaultID   string
	defaultTier string
	// adaptive marks a `-tiered` row: its thinking budget is always omitted.
	adaptive bool
	// thinking reports that the vendor's own metadata advertises a thinking
	// level for an exact id.  When it does not, the id still goes out
	// verbatim but no budget is attached, so a model that never thinks is not
	// handed a field it may reject.
	thinking bool
}

// resolve maps a caller's effort onto the pair the envelope needs.
func (r modelRoute) resolve(effort string) (upstream, tier string) {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if len(r.efforts) > 0 {
		if id, ok := r.efforts[effort]; ok {
			return id, effort
		}
		return r.defaultID, r.defaultTier
	}
	if r.exact == "" {
		return "", ""
	}
	if r.adaptive {
		return r.exact, "tiered"
	}
	if r.thinking {
		return r.exact, effortToTier(effort)
	}
	if _, level, ok := splitEffortVariant(r.exact); ok {
		// The id itself names a level the vendor serves (a family with only
		// one variant does not fold), so the budget follows the id rather than
		// the caller's slider.
		return r.exact, level
	}
	// Nothing in the vendor's metadata says this id thinks, so only the
	// admission key is sent.
	return r.exact, ""
}

// resolvedModel is the pair the envelope needs: the upstream admission key and
// the thinking level whose budget the request carries.
type resolvedModel struct {
	upstream string
	tier     string
}

// staticFlashRoute is the compiled-in fallback.  Its bare id is not a valid
// upstream admission key, so every effort (including the default) carries the
// suffix; this is the behaviour the module had before the catalogue went live.
func staticFlashRoute() modelRoute {
	efforts := make(map[string]string, len(effortIDs))
	for _, effort := range effortIDs {
		efforts[effort] = upstreamFlash + "-" + effort
	}
	return modelRoute{
		efforts:     efforts,
		defaultID:   upstreamFlash + "-" + defaultEffort,
		defaultTier: defaultEffort,
	}
}

// fallbackCatalogue is the row served with no usable upstream answer: the
// module's own knowledge of the vendor's flash line.
func (c *Client) fallbackCatalogue() []core.Model {
	efforts := make([]string, len(effortIDs))
	copy(efforts, effortIDs)
	return []core.Model{{
		ID:      upstreamFlash,
		OwnedBy: clientName,
		Extra: map[string]any{
			"display_name":      "Gemini 3.8 Flash",
			"context_length":    contextWindow,
			"max_output_tokens": maxOutputTokens,
			"supported_efforts": efforts,
			"default_effort":    defaultEffort,
			"supports_image":    true,
			"source":            modelsSourceFallback,
			"fallback":          true,
		},
	}}
}

// cloneModels copies a list deeply enough that a caller cannot mutate the
// module's cache through the returned slice or its Extra maps.
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

// --- cache ------------------------------------------------------------------

// lastGoodLocked returns the newest list worth showing.  modelsMu must be held.
func (c *Client) lastGoodLocked() []core.Model {
	if len(c.models) > 0 {
		return cloneModels(c.models)
	}
	return c.fallbackCatalogue()
}

// cachedCatalogue is the last good list, never a network call.
func (c *Client) cachedCatalogue() []core.Model {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	return c.lastGoodLocked()
}

// catalogueIsFresh reports whether the cache may answer without a refresh.  It
// is the cheap half of freshCatalogue for callers that only need the verdict.
func (c *Client) catalogueIsFresh(now time.Time) bool {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	return len(c.models) > 0 && now.Sub(c.modelsAt) < modelsCacheTTL
}

// freshCatalogue returns a copy of the cache when it may answer without a
// refresh.
func (c *Client) freshCatalogue(now time.Time) ([]core.Model, bool) {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	if len(c.models) == 0 || now.Sub(c.modelsAt) >= modelsCacheTTL {
		return nil, false
	}
	return cloneModels(c.models), true
}

// beginModelsRefresh is the single-flight throttle.  It refuses while a
// refresh runs and while the previous attempt is still inside the TTL, so an
// unreachable vendor is not asked on every poll.
func (c *Client) beginModelsRefresh(now time.Time) bool {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	if c.modelsLoading {
		return false
	}
	if !c.modelsTried.IsZero() && now.Sub(c.modelsTried) < modelsCacheTTL {
		return false
	}
	c.modelsLoading = true
	c.modelsTried = now
	return true
}

func (c *Client) endModelsRefresh() {
	c.modelsMu.Lock()
	c.modelsLoading = false
	c.modelsMu.Unlock()
}

// --- core.Client ------------------------------------------------------------

// Models reports the catalogue without ever making the caller wait on the
// network: a fresh cache answers directly, a cold one starts a background
// refresh and the last good (or compiled-in) list answers in the meantime.
func (c *Client) Models(ctx context.Context) ([]core.Model, error) {
	c.ensure()
	if list, ok := c.freshCatalogue(c.now()); ok {
		return list, nil
	}
	c.warmCatalogue()
	return c.cachedCatalogue(), nil
}

// warmCatalogue starts one background refresh when the cache is stale.  It is
// a no-op while a list is fresh, while a refresh runs, and when there is no
// credential to ask with -- in which case the compiled-in row answers without
// counting an attempt and without an error the operator would have to dismiss.
// Status calls it on every panel poll, so the live catalogue is up within
// seconds of startup and no caller ever waits on the vendor.
func (c *Client) warmCatalogue() {
	now := c.now()
	if c.catalogueIsFresh(now) {
		return
	}
	if c.pool.firstReady(now, c.cfg.maxInFlight()) == nil {
		return
	}
	c.refreshModelsAsync()
}

// RefreshModels implements core.ModelRefresher: the synchronous path behind the
// panel's refresh button.  It bypasses the TTL, and a failure never empties the
// catalogue -- the last good list is returned alongside the error.
func (c *Client) RefreshModels(ctx context.Context) ([]core.Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ensure()
	acct := c.pool.firstReady(c.now(), c.cfg.maxInFlight())
	if acct == nil {
		// Nothing to authenticate with.  The compiled-in row is a usable
		// answer, so this is not an error the operator has to dismiss.
		return c.fallbackCatalogue(), nil
	}
	c.modelsMu.Lock()
	c.modelsTried = c.now()
	c.modelsMu.Unlock()
	return c.fetchAndStore(ctx, acct)
}

// refreshModelsAsync starts one background catalogue refresh for a cold cache.
func (c *Client) refreshModelsAsync() {
	if !c.beginModelsRefresh(c.now()) {
		return
	}
	core.GoSafe(clientName+" models refresh", func(msg string) { c.noteError(msg) }, func() {
		defer c.endModelsRefresh()
		acct := c.pool.firstReady(c.now(), c.cfg.maxInFlight())
		if acct == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.quotaTimeout())
		defer cancel()
		if _, err := c.fetchAndStore(ctx, acct); err != nil {
			c.deps.Log("gemini: model refresh: %v", err)
		}
	})
}

// fetchAndStore records one catalogue attempt.  It always returns a usable
// list: the fetched one on success, the last good one (or the fallback) on
// failure.
func (c *Client) fetchAndStore(ctx context.Context, acct *accountRecord) ([]core.Model, error) {
	list, routes, err := c.fetchCatalogue(ctx, acct)

	c.modelsMu.Lock()
	c.modelsTried = c.now()
	if err == nil && len(list) > 0 {
		c.models = list
		c.routes = routes
		c.modelsAt = c.now()
	}
	fallback := c.lastGoodLocked()
	c.modelsMu.Unlock()

	if err != nil {
		return fallback, err
	}
	return cloneModels(list), nil
}

// fetchCatalogue asks for the vendor's live list and, when that is refused,
// for the ids the quota buckets name.
func (c *Client) fetchCatalogue(ctx context.Context, acct *accountRecord) ([]core.Model, map[string]modelRoute, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.ensureFresh(ctx, acct); err != nil {
		// The vendor is the authority on whether the token still works; a
		// renewal failure must not blank the catalogue.
		c.deps.Log("gemini: could not renew the credential before a model-list request: %v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.quotaTimeout())
	defer cancel()

	project := c.projectFor(ctx, acct)
	body := map[string]any{"project": project}

	status, raw, err := c.doJSON(ctx, endpointSandbox, fetchAvailableModelsPath, acct.AccessToken, body, true)
	var firstErr error
	switch {
	case err != nil:
		firstErr = err
	case status < 200 || status >= 300:
		firstErr = errors.New(describeBody(status, raw))
	default:
		list, routes, perr := parseAvailableModels(raw)
		if perr == nil && len(list) > 0 {
			return list, routes, nil
		}
		if perr == nil {
			perr = errors.New("the model list was empty")
		}
		firstErr = perr
	}

	status, raw, err = c.doJSON(ctx, endpointSandbox, retrieveUserQuotaPath, acct.AccessToken, body, true)
	switch {
	case err != nil:
		return nil, nil, fmt.Errorf("gemini: the model list could not be read (%v); the quota fallback also failed (%v)", firstErr, err)
	case status < 200 || status >= 300:
		return nil, nil, fmt.Errorf("gemini: the model list could not be read (%v); the quota fallback answered %s", firstErr, describeBody(status, raw))
	}
	list, routes, perr := parseQuotaModels(raw)
	if perr != nil || len(list) == 0 {
		if perr == nil {
			perr = errors.New("the quota response carried no model")
		}
		return nil, nil, fmt.Errorf("gemini: the model list could not be read (%v); the quota fallback answered no usable models (%v)", firstErr, perr)
	}
	return list, routes, nil
}

// --- wire shapes ------------------------------------------------------------

// wireModel is one catalogue row after filtering and id resolution.
type wireModel struct {
	id              string
	displayName     string
	contextLength   int
	maxOutputTokens int
	supportsImage   *bool
	supportsThink   bool
	discovery       string
}

// availableModelsPayload is the fetchAvailableModels response.  The map key is
// the vendor's own id; the value may repeat it in "model" and carries the
// display metadata.
type availableModelsPayload struct {
	Models          map[string]json.RawMessage `json:"models"`
	AgentModelSorts []struct {
		Groups []struct {
			ModelIDs []string `json:"modelIds"`
		} `json:"groups"`
	} `json:"agentModelSorts"`
}

type availableModelWire struct {
	DisplayName      string `json:"displayName"`
	Model            string `json:"model"`
	MaxTokens        int    `json:"maxTokens"`
	MaxOutputTokens  int    `json:"maxOutputTokens"`
	SupportsImages   *bool  `json:"supportsImages"`
	SupportsThinking bool   `json:"supportsThinking"`
	Disabled         bool   `json:"disabled"`
}

// parseAvailableModels converts the catalogue response into served rows plus
// their routing table.
func parseAvailableModels(body []byte) ([]core.Model, map[string]modelRoute, error) {
	var payload availableModelsPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, nil, fmt.Errorf("the vendor's model list was not JSON: %w", err)
	}
	if len(payload.Models) == 0 {
		return nil, nil, errors.New("the vendor's model list was empty")
	}

	wires := make(map[string]wireModel, len(payload.Models))
	for key, raw := range payload.Models {
		var entry availableModelWire
		if json.Unmarshal(raw, &entry) != nil {
			continue
		}
		id := firstNonEmpty(strings.TrimSpace(entry.Model), strings.TrimSpace(key))
		if id == "" || hiddenModelID(id) || hiddenModelID(key) || entry.Disabled {
			continue
		}
		wires[key] = wireModel{
			id:              id,
			displayName:     strings.TrimSpace(entry.DisplayName),
			contextLength:   entry.MaxTokens,
			maxOutputTokens: entry.MaxOutputTokens,
			supportsImage:   entry.SupportsImages,
			supportsThink:   entry.SupportsThinking,
		}
	}
	if len(wires) == 0 {
		return nil, nil, errors.New("the vendor's model list carried no usable model")
	}
	return buildCatalogue(wires, orderModelKeys(wires, payload.AgentModelSorts))
}

// parseQuotaModels converts the quota fallback's buckets into served rows.  A
// bucket names its model under "modelId" (the shape the vendor uses today) or
// "bucketId" (the shape older answers used); both are accepted.
func parseQuotaModels(body []byte) ([]core.Model, map[string]modelRoute, error) {
	var payload struct {
		Buckets []json.RawMessage `json:"buckets"`
		Groups  []struct {
			Buckets []json.RawMessage `json:"buckets"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, nil, fmt.Errorf("the quota response was not JSON: %w", err)
	}
	raws := append([]json.RawMessage(nil), payload.Buckets...)
	for _, group := range payload.Groups {
		raws = append(raws, group.Buckets...)
	}

	var (
		entries []wireModel
		seen    = map[string]bool{}
	)
	for _, raw := range raws {
		var bucket struct {
			ModelID     string `json:"modelId"`
			BucketID    string `json:"bucketId"`
			DisplayName string `json:"displayName"`
		}
		if json.Unmarshal(raw, &bucket) != nil {
			continue
		}
		id := firstNonEmpty(strings.TrimSpace(bucket.ModelID), strings.TrimSpace(bucket.BucketID))
		if id == "" || hiddenModelID(id) || seen[id] {
			continue
		}
		seen[id] = true
		entries = append(entries, wireModel{
			id:          id,
			displayName: strings.TrimSpace(bucket.DisplayName),
			discovery:   modelsDiscoveryQuota,
		})
	}
	if len(entries) == 0 {
		return nil, nil, errors.New("the quota response carried no model")
	}
	models, routes := foldModels(entries)
	if len(models) == 0 {
		return nil, nil, errors.New("the quota response carried no usable model")
	}
	return models, routes, nil
}

// hiddenModelID reports the ids that are not for chat: the IDE's completion
// and tab-completion helpers.
func hiddenModelID(id string) bool {
	lower := strings.ToLower(strings.TrimSpace(id))
	return strings.HasPrefix(lower, "chat_") || strings.HasPrefix(lower, "tab_")
}

// orderModelKeys puts the ids the vendor's own picker names first, in that
// order, then the remaining ids in stable alphabetical order.
func orderModelKeys(wires map[string]wireModel, sorts []struct {
	Groups []struct {
		ModelIDs []string `json:"modelIds"`
	} `json:"groups"`
}) []string {
	// A picker entry may name either the map key or the resolved model id.
	keys := make([]string, 0, len(wires))
	for key := range wires {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return wires[keys[i]].id < wires[keys[j]].id
	})
	byID := make(map[string]string, len(wires))
	for _, key := range keys {
		lower := strings.ToLower(wires[key].id)
		if _, ok := byID[lower]; !ok {
			byID[lower] = key
		}
	}

	var (
		ordered []string
		used    = make(map[string]bool, len(wires))
	)
	for _, s := range sorts {
		for _, g := range s.Groups {
			for _, id := range g.ModelIDs {
				key := strings.TrimSpace(id)
				if _, ok := wires[key]; !ok {
					key = byID[strings.ToLower(key)]
				}
				if key == "" || used[key] {
					continue
				}
				used[key] = true
				ordered = append(ordered, key)
			}
		}
	}
	for _, key := range keys {
		if !used[key] {
			ordered = append(ordered, key)
		}
	}
	return ordered
}

// buildCatalogue turns ordered wire rows into the served catalogue: duplicate
// ids collapse, effort families fold, and each row gets its route.
func buildCatalogue(wires map[string]wireModel, order []string) ([]core.Model, map[string]modelRoute, error) {
	var (
		entries []wireModel
		seen    = map[string]bool{}
	)
	for _, key := range order {
		w, ok := wires[key]
		if !ok || seen[w.id] {
			continue
		}
		seen[w.id] = true
		entries = append(entries, w)
	}
	if len(entries) == 0 {
		return nil, nil, errors.New("the vendor's model list carried no usable model")
	}
	models, routes := foldModels(entries)
	if len(models) == 0 {
		return nil, nil, errors.New("the vendor's model list carried no usable model")
	}
	return models, routes, nil
}

// --- folding ----------------------------------------------------------------

// splitEffortVariant splits an id whose last word is a foldable effort level.
func splitEffortVariant(id string) (base, effort string, ok bool) {
	for _, candidate := range effortVariantOrder {
		suffix := "-" + candidate
		if len(id) > len(suffix) && strings.HasSuffix(id, suffix) {
			return id[:len(id)-len(suffix)], candidate, true
		}
	}
	return id, "", false
}

// foldModels collapses each low/medium/high family into one row.  A family
// with a single variant keeps the vendor's exact id; so does every id that is
// not an effort variant at all (including "-tiered").
func foldModels(entries []wireModel) ([]core.Model, map[string]modelRoute) {
	index := make(map[string]int, len(entries))
	for i, e := range entries {
		if _, dup := index[e.id]; !dup {
			index[e.id] = i
		}
	}

	// members[base] is the set of effort levels the list has for that base.
	members := map[string]map[string]bool{}
	for _, e := range entries {
		base, effort, ok := splitEffortVariant(e.id)
		if !ok {
			continue
		}
		if members[base] == nil {
			members[base] = map[string]bool{}
		}
		members[base][effort] = true
	}
	foldable := make(map[string]bool, len(members))
	for base, levels := range members {
		if len(levels) >= 2 {
			foldable[base] = true
			continue
		}
		// A lone variant still folds when the list also has the bare id.
		if _, ok := index[base]; ok {
			foldable[base] = true
		}
	}

	var (
		models  []core.Model
		routes  = map[string]modelRoute{}
		emitted = map[string]bool{}
	)
	emit := func(m core.Model, r modelRoute) {
		models = append(models, m)
		routes[m.ID] = r
	}
	for _, e := range entries {
		if base, _, ok := splitEffortVariant(e.id); ok && foldable[base] {
			if emitted[base] {
				continue
			}
			emitted[base] = true
			m, r := foldFamily(base, entries, index, members[base])
			emit(m, r)
			continue
		}
		if foldable[e.id] {
			if emitted[e.id] {
				continue
			}
			emitted[e.id] = true
			m, r := foldFamily(e.id, entries, index, members[e.id])
			emit(m, r)
			continue
		}
		m, r := exactModel(e, entries)
		emit(m, r)
	}
	return models, routes
}

// foldFamily builds the single row for one effort family.  Metadata comes from
// the variant a no-effort request would send (the bare id when the vendor has
// one, else high, else the highest there is), with the context and output
// budgets taking the smallest value any variant reports so the row never
// over-promises.
func foldFamily(base string, entries []wireModel, index map[string]int, levels map[string]bool) (core.Model, modelRoute) {
	efforts := make([]string, 0, len(effortVariantOrder))
	for _, level := range effortVariantOrder {
		if levels[level] {
			efforts = append(efforts, level)
		}
	}
	def := defaultVariantEffort(efforts)

	route := modelRoute{efforts: map[string]string{}, defaultID: base}
	if _, hasBare := index[base]; hasBare {
		// The bare id is a valid admission key and carries no explicit budget,
		// so the vendor's own default answers.
		route.defaultTier = ""
	} else {
		route.defaultID = base + "-" + def
		route.defaultTier = def
	}
	for _, level := range efforts {
		route.efforts[level] = base + "-" + level
	}

	source := entries[index[route.defaultID]]
	extra := map[string]any{
		"source":            modelsSourceUpstream,
		"supported_efforts": append([]string(nil), efforts...),
		"default_effort":    def,
	}
	if name := strings.TrimSpace(source.displayName); name != "" {
		extra["display_name"] = stripEffortLabel(name, sourceEffortLabel(route.defaultID, base))
	}
	context, output := source.contextLength, source.maxOutputTokens
	for _, level := range efforts {
		v := entries[index[base+"-"+level]]
		context = minPositive(context, v.contextLength)
		output = minPositive(output, v.maxOutputTokens)
	}
	if context > 0 {
		extra["context_length"] = context
	}
	if output > 0 {
		extra["max_output_tokens"] = output
	}
	if source.supportsImage != nil {
		extra["supports_image"] = *source.supportsImage
	}

	return core.Model{ID: base, OwnedBy: clientName, Extra: extra}, route
}

// exactModel builds the row for an id that stands alone.
func exactModel(e wireModel, entries []wireModel) (core.Model, modelRoute) {
	extra := map[string]any{"source": modelsSourceUpstream}
	if e.discovery != "" {
		extra["discovery"] = e.discovery
	}
	if name := strings.TrimSpace(e.displayName); name != "" {
		extra["display_name"] = name
	}
	if e.contextLength > 0 {
		extra["context_length"] = e.contextLength
	}
	if e.maxOutputTokens > 0 {
		extra["max_output_tokens"] = e.maxOutputTokens
	}
	if e.supportsImage != nil {
		extra["supports_image"] = *e.supportsImage
	}
	if e.supportsThink {
		extra["supports_thinking"] = true
	}

	route := modelRoute{exact: e.id, thinking: e.supportsThink}
	if strings.HasSuffix(e.id, "-tiered") {
		// The tiered id thinks at the level the request asks, so it inherits
		// its base's effort ladder for display while always forwarding the
		// tiered id itself.
		route.adaptive = true
		if ladder := siblingEfforts(e.id, entries); len(ladder) > 0 {
			extra["supported_efforts"] = ladder
			extra["default_effort"] = defaultVariantEffort(ladder)
		}
	}
	return core.Model{ID: e.id, OwnedBy: clientName, Extra: extra}, route
}

// siblingEfforts is the low/medium/high ladder of a `-tiered` id's base.
func siblingEfforts(id string, entries []wireModel) []string {
	base := strings.TrimSuffix(id, "-tiered")
	present := map[string]bool{}
	for _, e := range entries {
		if b, effort, ok := splitEffortVariant(e.id); ok && b == base {
			present[effort] = true
		}
	}
	var out []string
	for _, level := range effortVariantOrder {
		if present[level] {
			out = append(out, level)
		}
	}
	return out
}

// defaultVariantEffort prefers high (what Gemini 3 thinks at when it is not
// told a level), then the highest level there is.
func defaultVariantEffort(efforts []string) string {
	if len(efforts) == 0 {
		return ""
	}
	for _, level := range efforts {
		if level == "high" {
			return level
		}
	}
	return efforts[len(efforts)-1]
}

// sourceEffortLabel is the level a family's default id carries, "" for a bare
// id.
func sourceEffortLabel(id, base string) string {
	if id == base {
		return ""
	}
	return strings.TrimPrefix(id, base+"-")
}

// stripEffortLabel removes a trailing level word from a display name, so
// "Gemini 3.7 Flash (High)" folds to "Gemini 3.7 Flash".
func stripEffortLabel(name, effort string) string {
	name = strings.TrimSpace(name)
	if name == "" || effort == "" {
		return name
	}
	lower := strings.ToLower(name)
	for _, suffix := range []string{" (" + effort + ")", " [" + effort + "]", " - " + effort, " " + effort, "-" + effort} {
		if strings.HasSuffix(lower, suffix) {
			return strings.TrimSpace(name[:len(name)-len(suffix)])
		}
	}
	return name
}

// minPositive returns the smallest positive value, treating 0 as "not said".
func minPositive(current, next int) int {
	if next <= 0 {
		return current
	}
	if current <= 0 || next < current {
		return next
	}
	return current
}

// --- routing ----------------------------------------------------------------

// modelIDs is the bare id list Status renders.  It reads the cache only.
func (c *Client) modelIDs() []string {
	list := c.cachedCatalogue()
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.ID)
	}
	return out
}

// routeFor resolves one advertised id onto its route.  The compiled-in flash
// row stays admissible after a dynamic refresh, so an existing client does not
// break when the vendor renames its catalogue.
func (c *Client) routeFor(model string) (modelRoute, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return modelRoute{}, false
	}
	c.modelsMu.Lock()
	route, ok := c.routes[model]
	if !ok {
		for id, candidate := range c.routes {
			if strings.EqualFold(id, model) {
				route, ok = candidate, true
				break
			}
		}
	}
	c.modelsMu.Unlock()
	if ok {
		return route, true
	}
	if canonicalModelID(model) == upstreamFlash {
		return staticFlashRoute(), true
	}
	return modelRoute{}, false
}

// resolveModel turns a catalogue id plus the request's effort into the pair
// the envelope needs.
func (c *Client) resolveModel(model string, req *core.ChatRequest) (resolvedModel, bool) {
	route, ok := c.routeFor(model)
	if !ok {
		return resolvedModel{}, false
	}
	upstream, tier := route.resolve(requestEffort(req))
	if upstream == "" {
		return resolvedModel{}, false
	}
	return resolvedModel{upstream: upstream, tier: tier}, true
}
