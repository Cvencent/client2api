package codearts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"client2api/internal/core"
)

// models.go is the model catalogue: which models exist, which of them bill
// against the free quota, and how large an answer each one may produce.
//
// Two endpoints feed it:
//
//	GET <snap>/v1/model/builtin            the ordinary catalogue, signed
//	GET https://opengw.developer.huaweicloud.com/api/v1/gateway/config
//	                                       the free-quota ("benefit") models
//
// The second one is a plain unauthenticated GET on a different host, and its
// `result.models` array names the models that do not consume paid credit.  The
// distinction matters on the wire: a benefit model must be requested with the
// `maas_type: benefit` header inside the signature, and a model that is not a
// benefit model must not carry it.

const (
	// builtinModelsPath is the catalogue endpoint, relative to the snap origin.
	builtinModelsPath = "/v1/model/builtin"
	// defaultGatewayConfigURL is the free-quota model list.
	defaultGatewayConfigURL = "https://opengw.developer.huaweicloud.com/api/v1/gateway/config"
	// defaultModelsURL is the origin every request in this module goes to.
	defaultBaseURL = "https://snap-access.cn-north-4.myhuaweicloud.com"
	// chatAPIPath is the chat completions endpoint, relative to the snap origin.
	chatAPIPath = "/api/v2/chat/completions"
	// queueStatusPath is the concurrency-queue endpoint.
	queueStatusPath = "/api/v1/queue/status"

	// defaultUserAgent identifies this gateway.  The vendor does not validate
	// it, but a request with no user agent at all is the sort of thing an edge
	// rule treats differently.
	defaultUserAgent = "client2api/codearts"
)

// builtinModels is the fallback catalogue, used before the first successful
// refresh and whenever the catalogue endpoint is unreachable.  It is the list
// the reference implementation ships as its static model list.
var builtinModels = []string{
	"GLM-5.2",
	"GLM-5.1",
	"GLM-5",
	"glm-5.3-flash",
	"openpangu-2.0-flash",
	"openpangu-2.0-pro",
	"deepseek-v4-flash",
	"deepseek-v4-pro",
	"deepseek-v4.1-flash",
}

// codeartsBenefitFallback is what the module assumes bills against free quota
// when neither the live list nor the disk cache can be read.  The reference
// implementation carries the same two ids for the same reason: an operator
// whose gateway is temporarily unreachable should still get the free tier
// rather than silently paying.
var codeartsBenefitFallback = []string{"glm-5.3-flash", "deepseek-v4.1-flash"}

// contextWindows is the per-model context budget.  The vendor's catalogue does
// not always carry it, so the values measured in the reference implementation
// are used as a floor.
var contextWindows = map[string]int{
	"GLM-5.2":             202752,
	"glm-5.3-flash":       1048576,
	"deepseek-v4-flash":   1048576,
	"deepseek-v4-pro":     1048576,
	"deepseek-v4.1-flash": 1000000,
}

// reModelDate strips the trailing build stamp from a model id:
// `deepseek-v4-flash-0731` is the dated build of `deepseek-v4-flash`.
//
// The rule is deliberately narrow — exactly four digits, and only when the id
// has more than five characters — because a model id that legitimately ends in
// four digits would otherwise be mangled into a different model.
var reModelDate = regexp.MustCompile(`-\d{4}$`)

// normalizeModelID removes a trailing four-digit build stamp.
func normalizeModelID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 5 {
		return id
	}
	if !reModelDate.MatchString(id) {
		return id
	}
	return id[:len(id)-5]
}

// isVisionModel reports whether an id names a vision variant, which this
// module's text-only request path cannot drive.
func isVisionModel(id string) bool {
	return strings.Contains(id, "-VL-") || strings.HasSuffix(id, "-VL")
}

// ---------------------------------------------------------------------------
// response shapes
// ---------------------------------------------------------------------------

// builtinModelsResponse is the catalogue endpoint's answer.  The reference
// implementation reads `builtinModels`; a couple of spellings are tolerated
// because the endpoint has changed shape before.
type builtinModelsResponse struct {
	BuiltinModels []modelInfo `json:"builtinModels"`
	Models        []modelInfo `json:"models"`
	Result        *struct {
		BuiltinModels []modelInfo `json:"builtinModels"`
		Models        []modelInfo `json:"models"`
	} `json:"result"`
}

// modelInfo is one entry of the catalogue.
type modelInfo struct {
	ModelID       string `json:"model_id"`
	ModelName     string `json:"model_name"`
	ContextLength int    `json:"context_length"`
	MaxTokens     int    `json:"max_tokens"`
	MaxOutput     int    `json:"max_output_tokens"`
	Provider      string `json:"provider"`
}

// entries returns whichever list the response actually carried.
func (r *builtinModelsResponse) entries() []modelInfo {
	if len(r.BuiltinModels) > 0 {
		return r.BuiltinModels
	}
	if len(r.Models) > 0 {
		return r.Models
	}
	if r.Result != nil {
		if len(r.Result.BuiltinModels) > 0 {
			return r.Result.BuiltinModels
		}
		return r.Result.Models
	}
	return nil
}

// gatewayConfigResponse is the free-quota model list.
type gatewayConfigResponse struct {
	Result struct {
		Models []struct {
			ModelID   string `json:"model_id"`
			ModelName string `json:"model_name"`
		} `json:"models"`
	} `json:"result"`
	// Some deployments answer with the list at the top level.
	Models []struct {
		ModelID   string `json:"model_id"`
		ModelName string `json:"model_name"`
	} `json:"models"`
}

// ids returns the model ids the response carried, in order.
func (r *gatewayConfigResponse) ids() []string {
	var out []string
	for _, m := range r.Result.Models {
		out = append(out, m.ModelID)
	}
	for _, m := range r.Models {
		out = append(out, m.ModelID)
	}
	return out
}

// ---------------------------------------------------------------------------
// parsing
// ---------------------------------------------------------------------------

// parseModelCatalogue turns a catalogue response into the model list this
// module serves, deduplicating by normalised id and dropping vision variants.
func parseModelCatalogue(entries []modelInfo) []core.Model {
	var out []core.Model
	seen := make(map[string]bool)
	for _, e := range entries {
		raw := strings.TrimSpace(e.ModelID)
		if raw == "" {
			continue
		}
		if isVisionModel(raw) {
			continue
		}
		id := normalizeModelID(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := normalizeModelID(firstNonEmpty(e.ModelName, raw))
		out = append(out, newModel(id, name, e.ContextLength, firstNonZero(e.MaxOutput, e.MaxTokens), e.Provider))
	}
	return out
}

// parseBenefitIDs reads the free-quota model ids.
//
// Only ids that the normaliser leaves alone are recorded.  The dated and the
// undated spelling of a model are different backends with opposite
// benefit-ness, so rewriting `deepseek-v4-flash-0731` into
// `deepseek-v4-flash` would mark the paid model as free — the exact mistake
// the reference implementation's comment warns about.
func parseBenefitIDs(raw []string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, id := range raw {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		if normalizeModelID(id) != id {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// newModel builds one core.Model, projecting the budget into Extra so the
// gateway can find it through core.ModelOutputLimit.
func newModel(id, name string, contextLength, maxOutput int, ownedBy string) core.Model {
	if contextLength <= 0 {
		contextLength = contextWindows[id]
	}
	if contextLength <= 0 {
		contextLength = defaultContextLength
	}
	if maxOutput <= 0 || maxOutput > maxOutputTokens {
		maxOutput = maxOutputTokens
	}
	extra := map[string]any{
		"context_length":    contextLength,
		"max_output_tokens": maxOutput,
	}
	if name != "" && name != id {
		extra["display_name"] = name
	}
	return core.Model{ID: id, OwnedBy: firstNonEmpty(ownedBy, "codearts"), Extra: extra}
}

// fallbackCatalogue is the list served before the first successful refresh.
func (c *Client) fallbackCatalogue() []core.Model {
	var out []core.Model
	for _, id := range c.cfg.fallbackModels() {
		out = append(out, newModel(id, "", 0, 0, ""))
	}
	return out
}

// ---------------------------------------------------------------------------
// fetching
// ---------------------------------------------------------------------------

// fetchCatalogue performs both catalogue requests and returns the models plus
// the free-quota ids.
//
// The benefit list is best-effort: a failure there must not cost the operator
// the model list, so its error is reported separately and the caller keeps
// whatever it had.
func (c *Client) fetchCatalogue(ctx context.Context, acct account) ([]core.Model, []string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.modelsTimeout())
	defer cancel()

	var resp builtinModelsResponse
	// The catalogue endpoint takes the same unsigned Agent-Type / X-Language
	// pair the reference implementation sends.
	err := c.doJSON(ctx, acct, http.MethodGet, c.cfg.modelsURL(), nil, nil, map[string]string{
		"Content-Type": "application/json",
		"Agent-Type":   "PromptCenter",
		"X-Language":   "zh-cn",
	}, &resp)
	if err != nil {
		return nil, nil, err
	}
	models := parseModelCatalogue(resp.entries())
	if len(models) == 0 {
		return nil, nil, errors.New("codearts: the catalogue endpoint returned no usable models")
	}

	benefit, berr := c.fetchBenefitIDs(ctx)
	if berr != nil {
		c.deps.Log("codearts: fetching the free-quota model list: %v", berr)
	}
	return models, benefit, nil
}

// fetchBenefitIDs reads the free-quota model list.  It is unauthenticated, so
// it needs no credential.
func (c *Client) fetchBenefitIDs(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.gatewayConfigURL(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.cfg.userAgent())
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("codearts: the free-quota list answered HTTP %d", resp.StatusCode)
	}
	var out gatewayConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("codearts: the free-quota list is not the JSON it should be: %w", err)
	}
	return parseBenefitIDs(out.ids()), nil
}

// ---------------------------------------------------------------------------
// cache
// ---------------------------------------------------------------------------

// modelsCache is what is persisted in models.json.
type modelsCache struct {
	Models    []core.Model `json:"models"`
	Benefit   []string     `json:"benefit_models"`
	UpdatedAt int64        `json:"updated_at"`
	// Version lets a future schema change be detected rather than misread.
	Version int `json:"version"`
}

// loadModelsCache reads the persisted catalogue.  A missing or unreadable file
// is not an error: the fallback list covers it.
func (c *Client) loadModelsCache() {
	if c.modelsPath == "" {
		return
	}
	var cache modelsCache
	if err := core.ReadJSON(c.modelsPath, &cache); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			c.deps.Log("codearts: reading %s: %v", c.modelsPath, err)
		}
		return
	}
	if len(cache.Models) == 0 {
		return
	}
	c.modelsMu.Lock()
	c.models = cache.Models
	c.modelsAt = time.Unix(cache.UpdatedAt, 0)
	c.benefit = cache.Benefit
	c.modelsMu.Unlock()
}

// saveModelsCache persists the catalogue.  A failure is logged, never fatal:
// the cache is an optimisation, not state.
func (c *Client) saveModelsCache(models []core.Model, benefit []string, at time.Time) {
	if c.modelsPath == "" {
		return
	}
	cache := modelsCache{Models: models, Benefit: benefit, UpdatedAt: at.Unix(), Version: 1}
	if err := core.WriteJSONAtomic(c.modelsPath, cache); err != nil {
		c.deps.Log("codearts: writing %s: %v", c.modelsPath, err)
	}
}

// cachedModels answers from memory, falling back to the built-in list.  It
// never blocks and never touches the network: core.Registry.Resolve calls
// Models for every bare model name, so this path has to be cheap.
func (c *Client) cachedModels() []core.Model {
	c.modelsMu.Lock()
	if len(c.models) > 0 {
		out := append([]core.Model(nil), c.models...)
		c.modelsMu.Unlock()
		return out
	}
	c.modelsMu.Unlock()
	return c.fallbackCatalogue()
}

// modelsFresh reports whether the cached catalogue is still inside its TTL.
func (c *Client) modelsFresh(now time.Time) bool {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	if len(c.models) == 0 || c.modelsAt.IsZero() {
		return false
	}
	return now.Sub(c.modelsAt) < c.cfg.modelsTTL()
}

// isBenefitModel reports whether an id bills against the free quota.
//
// The order is the reference implementation's: what the live list said, then
// what the disk cache said, then the built-in fallback.  A model that no list
// mentions is NOT a benefit model — guessing "free" would send the
// `maas_type: benefit` header for a paid model and get it refused.
func (c *Client) isBenefitModel(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	c.modelsMu.Lock()
	benefit := append([]string(nil), c.benefit...)
	c.modelsMu.Unlock()
	if len(benefit) == 0 {
		benefit = codeartsBenefitFallback
	}
	for _, id := range benefit {
		if strings.EqualFold(id, model) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// refresh
// ---------------------------------------------------------------------------

// refreshModelsAsync kicks a catalogue refresh in the background, at most one
// at a time and no more often than the retry interval after a failure.
func (c *Client) refreshModelsAsync() {
	c.modelsMu.Lock()
	if c.modelsLoading {
		c.modelsMu.Unlock()
		return
	}
	if !c.modelsRetryAt.IsZero() && time.Now().Before(c.modelsRetryAt) {
		c.modelsMu.Unlock()
		return
	}
	c.modelsLoading = true
	c.modelsMu.Unlock()

	core.GoSafe("codearts models", nil, func() {
		defer func() {
			c.modelsMu.Lock()
			c.modelsLoading = false
			c.modelsMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.modelsTimeout()+5*time.Second)
		defer cancel()
		if _, err := c.RefreshModels(ctx); err != nil {
			c.modelsMu.Lock()
			c.modelsRetryAt = time.Now().Add(modelsRetryInterval)
			c.modelsMu.Unlock()
			c.deps.Log("codearts: refreshing the model catalogue: %v", err)
		}
	})
}

// modelsRetryInterval bounds how often a failing refresh is retried.
const modelsRetryInterval = 30 * time.Second

// RefreshModels fetches the catalogue synchronously (core.ModelRefresher).
//
// On failure the last known list is returned *alongside* the error, so one
// flaky refresh never empties the model picker.
func (c *Client) RefreshModels(ctx context.Context) ([]core.Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	e := c.pool.pick(nil)
	if e == nil {
		return c.cachedModels(), core.ErrNotConfigured
	}
	models, benefit, err := c.fetchCatalogue(ctx, e.account())
	if err != nil {
		kind, msg := classifyErr(err)
		c.pool.markFailure(e, kind, msg)
		return c.cachedModels(), fmt.Errorf("codearts: refresh models: %s", cleanErrorText(msg))
	}
	c.pool.markUsed(e)

	now := time.Now()
	c.modelsMu.Lock()
	c.models = models
	c.modelsAt = now
	c.modelsRetryAt = time.Time{}
	if len(benefit) > 0 {
		c.benefit = benefit
	}
	c.modelsMu.Unlock()
	c.saveModelsCache(models, benefit, now)
	return append([]core.Model(nil), models...), nil
}

// ModelMaxOutputTokens answers the per-model output budget
// (core.ModelLimitsProvider).
//
// It is answered from the cache only, and never guesses: `ok=false` makes the
// gateway send no cap at all, which is strictly better than a cap that is
// either too high (a vendor-side rejection) or too low (silent truncation).
func (c *Client) ModelMaxOutputTokens(ctx context.Context, model string) (int, bool) {
	return core.OutputLimitFor(c.cachedModels(), model)
}
