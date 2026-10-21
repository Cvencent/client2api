package codearts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
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
// The second one is a SIGNED GET on a different host, and its `result.models`
// array names the models that do not consume paid credit.  Both the channel
// check and the list must be signed: the unauthenticated call answers 401.  The
// distinction matters on the wire: a benefit model must be requested with the
// `maas_type: benefit` header inside the signature, and a model that is not a
// benefit model must not carry it -- signing it for a paid model is refused
// with `InferHub.4004.200 benefit not found`.

const (
	// builtinModelsPath is the catalogue endpoint, relative to the snap origin.
	builtinModelsPath = "/v1/model/builtin"
	// defaultGatewayConfigURL is the free-quota model list.  It is only
	// meaningful once the benefit channel precheck says the channel is on, and
	// it must be requested WITH a signature: the unsigned call answers 401.
	defaultGatewayConfigURL = "https://opengw.developer.huaweicloud.com/api/v1/gateway/config"
	// benefitGatewayConfigPath is the channel precheck.  The vendor's own client
	// asks this first and skips the whole benefit channel when it is disabled,
	// because a benefit request for a disabled channel is refused outright with
	// `InferHub.4004.200 benefit not found`.
	benefitGatewayConfigPath = "/v1/benefit-gateway-config"
	// educationGatewayConfigPath is the sibling channel.  Both are consulted so a
	// credential that only has education entitlement still gets its models.
	educationGatewayConfigPath = "/v1/education-gateway-config"
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

// There is deliberately NO built-in list of "probably free" models.
//
// An earlier version carried one (glm-5.3-flash, deepseek-v4.1-flash) so an
// operator whose gateway was unreachable would "still get the free tier".  It
// did the opposite: the vendor gates the benefit channel per credential, and
// when that channel is off every request carrying `maas_type: benefit` is
// refused with `InferHub.4004.200 benefit not found` -- including models the
// catalogue sells normally and which answer fine WITHOUT the header.  A guess
// therefore turned two working models into two broken ones.

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
	ModelID       string              `json:"model_id"`
	ModelName     string              `json:"model_name"`
	ContextLength int                 `json:"context_length"`
	MaxTokens     int                 `json:"max_tokens"`
	MaxOutput     int                 `json:"max_output_tokens"`
	Provider      string              `json:"provider"`
	Credit        []creditEntry       `json:"credit"`
	TimeSlots     map[string]timeSlot `json:"time_slots"`
}

// timeSlot is one entry of the vendor's time_slots table.  A slot with no
// schedule (the "default" one) is always in force; a promotional slot names a
// cron-like schedule plus how long each occurrence lasts.
type timeSlot struct {
	Timezone string   `json:"timezone"`
	Schedule *string  `json:"schedule"`
	Duration *float64 `json:"duration"`
	TimeUnit string   `json:"timeunit"`
}

// creditEntry is one row of the vendor's credit table.  `ratio_display` is the
// 倍率 the vendor's own client shows ("0.18x"); `ratio` is an internal credit
// factor on a different scale and must not be substituted for it.  A row is
// scoped to a time slot (`time_slot_id`) and/or an input-token range.
type creditEntry struct {
	Ratio              string `json:"ratio"`
	RatioDisplay       string `json:"ratio_display"`
	BeforeDisplayRatio string `json:"before_display_ratio"`
	InputPrice         string `json:"input_price"`
	CachePrice         string `json:"cache_price"`
	OutputPrice        string `json:"output_price"`
	TimeSlotID         string `json:"time_slot_id"`
	InputFrom          int    `json:"input_from"`
	InputTo            int    `json:"input_to"`
	OutputFrom         int    `json:"output_from"`
	OutputTo           int    `json:"output_to"`
}

// vendorZone is the timezone the vendor quotes its promotions in.  A
// FixedZone is used rather than time.LoadLocation: Windows ships no IANA
// database, and every slot the vendor publishes is UTC+8 without DST.
var vendorZone = time.FixedZone("CST", 8*3600)

// multiplierFromCredits returns the list 倍率 and the 倍率 in force at now.
//
// The vendor publishes one row per (time slot, input range).  Rows in the
// `default` slot are the list price; a promotional slot such as `off_peak`
// only applies inside its own window.  Advertising the night rate at noon would
// be a lie, so the effective value is resolved against the vendor's schedule.
func multiplierFromCredits(credits []creditEntry, slots map[string]timeSlot, now time.Time) (list, effective float64, ok bool) {
	var listSet, effSet bool
	for _, c := range credits {
		slot := strings.TrimSpace(c.TimeSlotID)
		if slot != "" && !strings.EqualFold(slot, "default") {
			continue
		}
		if v, ok := parseMultiplier(c.RatioDisplay); ok {
			list, listSet = v, true
			break
		}
	}
	if !listSet {
		return 0, 0, false
	}
	effective = list
	// Prefer the promotional slot whose window covers `now`; the deepest
	// discount wins if the vendor ever overlaps two of them.
	for _, c := range credits {
		slot := strings.TrimSpace(c.TimeSlotID)
		if slot == "" || strings.EqualFold(slot, "default") {
			continue
		}
		v, ok := parseMultiplier(c.RatioDisplay)
		if !ok || !slotActive(slots[slot], now) {
			continue
		}
		if !effSet || v < effective {
			effective, effSet = v, true
		}
	}
	return list, effective, true
}

// slotActive reports whether a promotional slot is in force at now.
//
// A slot with no schedule is NOT assumed to be running: the vendor only names
// the window it publishes, and claiming an unproven discount is worse than
// showing the list price.  The "default" slot is skipped by the caller, so
// everything reaching here is promotional.
//
// The schedule is a six-field cron
// ("0 0 21 * * *" = 21:00 daily); only the fields the vendor actually varies
// are honoured -- minute, hour and day-of-month/month are matched literally and
// any other value is treated as "every".  A duration extends each occurrence,
// so 21:00 + 11h covers 21:00-07:59 across midnight.
func slotActive(s timeSlot, now time.Time) bool {
	if s.Schedule == nil || strings.TrimSpace(*s.Schedule) == "" {
		return false
	}
	loc := vendorZone
	if tz := strings.TrimSpace(s.Timezone); tz != "" && tz != "Asia/Shanghai" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	local := now.In(loc)
	// Match the schedule against every day the window could still cover: a
	// window that opened yesterday evening may still be running now.
	span := time.Duration(0)
	if s.Duration != nil && *s.Duration > 0 {
		span = slotDuration(*s.Duration, s.TimeUnit)
	}
	days := int(span/(24*time.Hour)) + 1
	for d := 0; d <= days; d++ {
		day := local.AddDate(0, 0, -d)
		start, ok := slotStart(*s.Schedule, day, loc)
		if !ok {
			continue
		}
		if !local.Before(start) && local.Before(start.Add(spanOrDay(span))) {
			return true
		}
	}
	return false
}

// spanOrDay is the effective length of an occurrence.  A slot with no duration
// is treated as covering the rest of its day.
func spanOrDay(span time.Duration) time.Duration {
	if span <= 0 {
		return 24 * time.Hour
	}
	return span
}

// slotDuration converts the vendor's duration+unit pair.
func slotDuration(n float64, unit string) time.Duration {
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "minute", "minutes", "分", "分钟":
		return time.Duration(n * float64(time.Minute))
	case "second", "seconds", "秒":
		return time.Duration(n * float64(time.Second))
	case "day", "days", "天":
		return time.Duration(n * 24 * float64(time.Hour))
	}
	return time.Duration(n * float64(time.Hour))
}

// slotStart resolves a six-field cron against one local day.  Only minute,
// hour and day-of-month/month are honoured; "*" or "?" means "any".
func slotStart(expr string, day time.Time, loc *time.Location) (time.Time, bool) {
	f := strings.Fields(expr)
	if len(f) < 5 {
		return time.Time{}, false
	}
	// Six-field form is "sec min hour dom mon dow"; five-field drops the seconds.
	var sec, min, hour, dom, mon string
	if len(f) >= 6 {
		sec, min, hour, dom, mon = f[0], f[1], f[2], f[3], f[4]
	} else {
		sec, min, hour, dom, mon = "0", f[0], f[1], f[2], f[3]
	}
	d, ok := cronField(dom, day.Day())
	if !ok {
		return time.Time{}, false
	}
	m, ok := cronField(mon, int(day.Month()))
	if !ok {
		return time.Time{}, false
	}
	secN, ok1 := cronValue(sec, 0, 59)
	minN, ok2 := cronValue(min, 0, 59)
	hourN, ok3 := cronValue(hour, 0, 23)
	if !ok1 || !ok2 || !ok3 {
		return time.Time{}, false
	}
	return time.Date(day.Year(), time.Month(m), d, hourN, minN, secN, 0, loc), true
}

// cronField matches one numeric cron field.
func cronField(field string, want int) (int, bool) {
	field = strings.TrimSpace(field)
	if field == "*" || field == "?" || field == "" {
		return want, true
	}
	v, ok := cronValue(field, 0, 0)
	if !ok {
		return 0, false
	}
	return v, v == want
}

// cronValue reads a literal cron field.
func cronValue(field string, min, max int) (int, bool) {
	field = strings.TrimSpace(field)
	if field == "*" || field == "?" || field == "" {
		return min, true
	}
	n, err := strconv.Atoi(field)
	if err != nil || n < min || n > max {
		return 0, false
	}
	return n, true
}

// parseMultiplier reads the vendor's display rate ("0.18x", "0.7x", "免费").
func parseMultiplier(s string) (float64, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, false
	}
	if s == "免费" || s == "free" {
		return 0, true
	}
	s = strings.TrimSpace(strings.TrimSuffix(s, "x"))
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// creditMultiplierIsZero reports whether a model's advertised 倍率 is zero.
func creditMultiplierIsZero(extra map[string]any) bool {
	if extra == nil {
		return false
	}
	for _, key := range []string{"billing_effective_multiplier", "billing_multiplier"} {
		switch v := extra[key].(type) {
		case float64:
			if v == 0 {
				return true
			}
		case int:
			if v == 0 {
				return true
			}
		}
	}
	return false
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
	// ErrorCode carries the vendor's business verdict.  HTTP 200 with a non-zero
	// code means "no channel configured", which is a definitive empty list.
	ErrorCode string `json:"error_code"`
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
	return parseModelCatalogueAt(entries, time.Now())
}

// parseModelCatalogueAt is parseModelCatalogue with an explicit clock, so the
// promotional window can be exercised without waiting for 21:00.
func parseModelCatalogueAt(entries []modelInfo, now time.Time) []core.Model {
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
		m := newModel(id, name, e.ContextLength, firstNonZero(e.MaxOutput, e.MaxTokens), e.Provider)
		if list, eff, ok := multiplierFromCredits(e.Credit, e.TimeSlots, now); ok {
			if m.Extra == nil {
				m.Extra = map[string]any{}
			}
			m.Extra["billing_multiplier"] = list
			m.Extra["billing_effective_multiplier"] = eff
			if eff == 0 {
				m.Extra["free"] = true
			}
		}
		out = append(out, m)
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
	return markBenefitModels(out, c.benefitIDsLocked())
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

	benefit, berr := c.fetchBenefitIDs(ctx, acct)
	if berr != nil {
		c.deps.Log("codearts: fetching the free-quota model list: %v", berr)
	}
	return models, benefit, nil
}

// fetchBenefitIDs reads the free-quota model ids for this credential.
//
// The vendor gates this behind a per-credential channel check, and BOTH calls
// must be signed: the unsigned model-list call answers
// `401 APIG.0301 x-auth-token not found`, which is why the old unauthenticated
// reader never learned anything and the built-in fallback silently took over.
//
// A disabled (or unreadable) channel yields an empty list, and an empty list
// must mean "no benefit models" -- not "fall back to guessing".  Guessing is
// what poisons a working model: a paid request that carries
// `maas_type: benefit` is refused with `InferHub.4004.200 benefit not found`,
// while the identical request without the header succeeds.
func (c *Client) fetchBenefitIDs(ctx context.Context, acct account) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	enabled, err := c.benefitChannelEnabled(ctx, acct)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.gatewayConfigURL(), nil)
	if err != nil {
		return nil, err
	}
	signed, err := signRequest(acct.AccessKeyID, acct.SecretAccessKey, acct.SecurityToken, http.MethodGet, c.cfg.gatewayConfigURL(), nil, nil, time.Now())
	if err != nil {
		return nil, err
	}
	for k, v := range signedRequestHeaders(signed) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Agent-Type", "PromptCenter")
	req.Header.Set("X-Language", "zh-cn")
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
	// The endpoint answers HTTP 200 with a business error when no channel is
	// configured; that is a definitive "no benefit models", not a failure.
	if code := strings.TrimSpace(out.ErrorCode); code != "" && code != "0000" {
		return nil, nil
	}
	return parseBenefitIDs(out.ids()), nil
}

// benefitChannelEnabled asks the credential's policy centre whether the
// benefit channel is switched on.  A channel that is off has no models, and
// requesting one anyway is what produces `benefit not found`.
func (c *Client) benefitChannelEnabled(ctx context.Context, acct account) (bool, error) {
	rawURL := strings.TrimRight(c.cfg.policyURL(), "/") + benefitGatewayConfigPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false, err
	}
	signed, err := signRequest(acct.AccessKeyID, acct.SecretAccessKey, acct.SecurityToken, http.MethodGet, rawURL, nil, nil, time.Now())
	if err != nil {
		return false, err
	}
	for k, v := range signedRequestHeaders(signed) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Agent-Type", "PromptCenter")
	req.Header.Set("X-Language", "zh-cn")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.cfg.userAgent())
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("codearts: the benefit channel check answered HTTP %d", resp.StatusCode)
	}
	var out struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, fmt.Errorf("codearts: the benefit channel check is not the JSON it should be: %w", err)
	}
	return out.Enabled != nil && *out.Enabled, nil
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
	benefit := cache.Benefit
	c.modelsMu.Lock()
	c.models = markBenefitModels(cache.Models, benefit)
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
	// An empty list is the vendor saying "no benefit models".  It is NOT a
	// reason to fall back to a guessed list: a paid model that carries
	// `maas_type: benefit` is refused with InferHub.4004.200, and that refusal
	// costs the caller a model that would otherwise answer normally.
	for _, id := range benefit {
		if strings.EqualFold(id, model) {
			return true
		}
	}
	return false
}

// benefitIDsLocked is the free-quota list in force: the live list, then the
// disk cache, then the built-in fallback.  The caller holds modelsMu.
func (c *Client) benefitIDsLocked() []string {
	return append([]string(nil), c.benefit...)
}

// markBenefitModels stamps `extra.free` on the models that bill against the
// free quota.  The gateway reads that flag to mark a request as free, and the
// module's account guard steps aside for those: a benefit request spends no
// credit, so a spent wallet must not hide it.  The list is the same one
// isBenefitModel uses, so the flag and the `maas_type: benefit` header can
// never disagree.
// The stamp is authoritative in BOTH directions: a model the vendor no longer
// lists must lose the flag.  A catalogue cached before the channel was
// disabled still carries `free: true`, and leaving it there would keep signing
// `maas_type: benefit` for a model the vendor now refuses with
// `InferHub.4004.200 benefit not found`.
func markBenefitModels(models []core.Model, benefit []string) []core.Model {
	free := make(map[string]bool, len(benefit))
	for _, id := range benefit {
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
			free[id] = true
		}
	}
	for i := range models {
		id := strings.ToLower(strings.TrimSpace(models[i].ID))
		// Two independent sources can prove a model spends no credit: the
		// vendor's benefit list, and a zero 倍率 in its own price table.
		if free[id] || creditMultiplierIsZero(models[i].Extra) {
			if models[i].Extra == nil {
				models[i].Extra = map[string]any{}
			}
			models[i].Extra["free"] = true
			continue
		}
		// Drop a stale stamp rather than leaving it in place.
		if models[i].Extra != nil {
			delete(models[i].Extra, "free")
		}
	}
	return models
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
	// The vendor's answer replaces the previous one even when it is empty:
	// "this channel is off" is a fact, not a missing value.
	c.benefit = benefit
	models = markBenefitModels(models, c.benefitIDsLocked())
	c.models = models
	c.modelsAt = now
	c.modelsRetryAt = time.Time{}
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
