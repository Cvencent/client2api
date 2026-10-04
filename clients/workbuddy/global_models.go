package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// globalModelsProbeTimeout bounds one whole global-catalogue probe: the three
// parallel legs (two /v3/config user agents plus the ordered enterprise path
// family) must all finish inside it or the fetch gives up.  Without it a
// half-open connection to a vendor endpoint keeps the caller blocked on
// wg.Wait() long past the point where the answer stopped mattering.
const globalModelsProbeTimeout = 60 * time.Second

// Global (international realm) model discovery, ported from the reference
// internal/upstream/global_models.go (MIT).
//
// Two things make the global catalogue different from the CN one, and both were
// measured rather than guessed:
//
//   - The enterprise probe has TWO paths.  `gpt-5.3-codex` is served only by
//     /v2/enterprises/personal/models; the /console spelling answers 200 with a
//     catalogue that simply does not contain it.  Probing only one path loses a
//     model without any error, so both are tried in order.
//   - /v3/config answers differently for the IDE and the CLI user agent, and the
//     two answers are not nested: the union is what the account can really use.
//
// The reference is deliberately pure-dynamic: it has no static fallback list at
// all, because a hard-coded id that the backend no longer serves only makes
// clients pick a model that answers 11102.  This module now matches that -- see
// the policy comment in workbuddy.go above the model constants: Models() and
// RefreshModels() serve the fetched catalogue, the operator's configured list,
// or nothing.

// globalModelsProbePaths is the enterprise probe family, in priority order.
var globalModelsProbePaths = []string{
	"/v2/enterprises/personal/models",
	"/console/enterprises/personal/models",
}

const (
	// globalModelsTTL is how long a successful global catalogue is reused.
	globalModelsTTL = time.Hour
	// globalModelsFailCooldown is how long a failed probe suppresses retries.
	// Probing on every request during an outage is what turns a partial outage
	// into a flood.
	globalModelsFailCooldown = 5 * time.Minute
)

// globalModelsCache is the global catalogue cache.  It is intentionally
// independent of the CN path: the two realms serve different catalogues, and a
// global probe result must never be served to a CN account (or vice versa).
type globalModelsCache struct {
	mu       sync.Mutex
	names    []string
	infos    []ModelInfo
	fetched  time.Time
	lastFail time.Time
}

// FetchGlobalModels returns the global catalogue's model ids, or nil when the
// realm has nothing to offer.  It never returns an error: "no catalogue" is a
// normal state for this call, and the caller has no repair to attempt.
func (u *Upstream) FetchGlobalModels(a *Auth) []string {
	names, _ := u.fetchGlobalModelsOnce(a)
	return names
}

// FetchGlobalModelInfos is FetchGlobalModels with the full model metadata.
func (u *Upstream) FetchGlobalModelInfos(a *Auth) []ModelInfo {
	_, infos := u.fetchGlobalModelsOnce(a)
	return infos
}

// fetchGlobalModelsOnce serves the cache when it is fresh, honours the failure
// cooldown, and otherwise probes and caches.  A non-global account returns
// (nil, nil) with zero upstream calls: the escape hatch must be free.
func (u *Upstream) fetchGlobalModelsOnce(a *Auth) ([]string, []ModelInfo) {
	if u == nil || !u.globalOn(a) {
		return nil, nil
	}
	u.globalModels.mu.Lock()
	names, infos := u.globalModels.names, u.globalModels.infos
	fetched, lastFail := u.globalModels.fetched, u.globalModels.lastFail
	u.globalModels.mu.Unlock()

	if len(names) > 0 && !fetched.IsZero() && time.Since(fetched) < globalModelsTTL {
		return copyNames(names), infos
	}
	if !lastFail.IsZero() && time.Since(lastFail) < globalModelsFailCooldown {
		return nil, nil
	}

	names, infos, efforts, defaults, err := u.probeGlobalModels(context.Background(), a)
	if err != nil || len(names) == 0 {
		u.globalModels.mu.Lock()
		u.globalModels.lastFail = time.Now()
		u.globalModels.mu.Unlock()
		if err != nil {
			u.log("workbuddy: global model probe failed: %v", err)
		}
		return nil, nil
	}
	if len(efforts) > 0 || len(defaults) > 0 {
		u.storeEfforts(realmGlobal, efforts, defaults)
	}
	names = dedupeNames(names)

	u.globalModels.mu.Lock()
	u.globalModels.names = names
	u.globalModels.infos = infos
	u.globalModels.fetched = time.Now()
	u.globalModels.lastFail = time.Time{}
	u.globalModels.mu.Unlock()

	return copyNames(names), infos
}

// probeGlobalModels runs the three probes concurrently and unions what came
// back.  Degrading to one source is logged, never hidden.
//
// ctx bounds the whole fan-out, and each probe carries it into its request, so a
// stalled upstream can no longer pin a goroutine past the caller's deadline:
// the wait below is on real HTTP calls that honour cancellation.
func (u *Upstream) probeGlobalModels(ctx context.Context, a *Auth) (names []string, infos []ModelInfo, efforts map[string][]string, defaults map[string]string, err error) {
	type v3result struct {
		names []string
		infos []ModelInfo
		err   error
	}
	type entResult struct {
		names []string
		infos []ModelInfo
		err   error
	}

	ctx, cancel := context.WithTimeout(ctx, globalModelsProbeTimeout)
	defer cancel()

	// GoSafe, not a bare "go": three probes run at once here, and one bad
	// payload must cost that probe, not the process.
	//
	// Each probe publishes by sending on its own buffered channel rather than
	// writing a shared struct and calling wg.Done.  core.GoSafe runs its report
	// only after fn's own defers, so a deferred wg.Done would release the
	// collector before a panic report had written anything -- and the zero
	// value left behind is indistinguishable from a probe that honestly found
	// no models, which would silently blank the catalogue.  Sending is the last
	// thing on both paths, so the receive below both orders the write and
	// carries the panic.
	ideCh := make(chan v3result, 1)
	cliCh := make(chan v3result, 1)
	entCh := make(chan entResult, 1)
	probeV3 := func(what, ua string, ch chan<- v3result) {
		core.GoSafe(what, func(msg string) {
			u.log("workbuddy: %s", msg)
			ch <- v3result{err: errors.New(msg)}
		}, func() {
			names, infos, err := u.probeGlobalV3(ctx, a, ua)
			ch <- v3result{names: names, infos: infos, err: err}
		})
	}
	probeV3("workbuddy global models probe (IDE UA)", codeBuddyIDEUA, ideCh)
	probeV3("workbuddy global models probe (CLI UA)", codeBuddyCLIUA, cliCh)
	core.GoSafe("workbuddy global enterprise models probe", func(msg string) {
		u.log("workbuddy: %s", msg)
		entCh <- entResult{err: errors.New(msg)}
	}, func() {
		names, infos, err := u.probeGlobalEnterprise(ctx, a)
		entCh <- entResult{names: names, infos: infos, err: err}
	})
	ide, cli := <-ideCh, <-cliCh
	ent := <-entCh
	entNames, entInfos, entErr := ent.names, ent.infos, ent.err

	// The two /v3/config answers are merged first: neither UA is a superset of
	// the other.
	v3Names, v3Infos := ide.names, ide.infos
	var v3Err error
	switch {
	case ide.err != nil && cli.err != nil:
		v3Err = ide.err
	case ide.err != nil:
		u.log("workbuddy: global models: v3/config IDE-UA probe failed (CLI-UA only): %v", ide.err)
		v3Names, v3Infos = cli.names, cli.infos
	case cli.err != nil:
		u.log("workbuddy: global models: v3/config CLI-UA probe failed (IDE-UA only): %v", cli.err)
	default:
		v3Names, v3Infos = mergeGlobalCatalog(ide.names, ide.infos, cli.names, cli.infos)
	}

	if v3Err != nil && entErr != nil {
		return nil, nil, nil, nil, v3Err
	}
	if v3Err != nil {
		u.log("workbuddy: global models: v3/config probe failed (degraded to enterprise endpoint): %v", v3Err)
		names, infos, efforts, defaults = extractEfforts(entInfos)
		return names, infos, efforts, defaults, nil
	}
	if entErr != nil {
		u.log("workbuddy: global models: enterprise endpoint failed (v3/config only): %v", entErr)
		names, infos, efforts, defaults = extractEfforts(v3Infos)
		return names, infos, efforts, defaults, nil
	}
	names, infos = mergeGlobalCatalog(v3Names, v3Infos, entNames, entInfos)
	_, _, v3Efforts, v3Defaults := extractEfforts(v3Infos)
	_, _, entEfforts, entDefaults := extractEfforts(entInfos)
	return names, infos, mergeEffortBuckets(v3Efforts, entEfforts), mergeEffortDefaults(v3Defaults, entDefaults), nil
}

// probeGlobalV3 reads one /v3/config answer, drops non-chat entries and sorts
// the ids so the union has a stable order.
func (u *Upstream) probeGlobalV3(ctx context.Context, a *Auth, ua string) ([]string, []ModelInfo, error) {
	byID, err := u.fetchV3ConfigModelMap(ctx, a, ua)
	if err != nil {
		return nil, nil, err
	}
	infos := make([]ModelInfo, 0, len(byID))
	for id, mi := range byID {
		if mi.ID == "" || nonChatModel(id, mi.MaxTokens, mi.Tags) {
			continue
		}
		infos = append(infos, mi)
	}
	if len(infos) == 0 {
		return nil, nil, errors.New("v3 config returned no chat models")
	}
	sort.SliceStable(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	names := make([]string, 0, len(infos))
	for _, mi := range infos {
		names = append(names, mi.ID)
	}
	return names, infos, nil
}

// probeGlobalEnterprise walks the ordered enterprise path family and keeps the
// first path that answers.  The last error is the one reported.
func (u *Upstream) probeGlobalEnterprise(ctx context.Context, a *Auth) ([]string, []ModelInfo, error) {
	var lastErr error
	for _, path := range globalModelsProbePaths {
		names, infos, err := u.globalModelsOnce(ctx, a, path)
		if err == nil && len(names) > 0 {
			return names, infos, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("global models: no enterprise path answered")
	}
	return nil, nil, lastErr
}

// globalModelsOnce reads and parses one enterprise probe path.
func (u *Upstream) globalModelsOnce(ctx context.Context, a *Auth, path string) ([]string, []ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.chatBase(a)+path, nil)
	if err != nil {
		return nil, nil, err
	}
	u.CommonHeaders(req, a)
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	}
	client := u.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, modelsCacheResponseLimit))
	if err != nil {
		return nil, nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("global models status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	return parseGlobalModelNames(raw)
}

// globalLooseEntry is the permissive shape used when the full dynModelEntry
// parse yields nothing: the global catalogue has been seen with camelCase window
// keys and a bare effort pair.
type globalLooseEntry struct {
	ID             string   `json:"id"`
	ModelID        string   `json:"modelId"`
	Model          string   `json:"model"`
	Name           string   `json:"name"`
	Disabled       bool     `json:"disabled"`
	ContextWindow  int64    `json:"contextWindow"`
	MaxInputTokens int64    `json:"maxInputTokens"`
	MaxOutputToken int64    `json:"maxOutputTokens"`
	MaxTokens      int64    `json:"maxTokens"`
	Efforts        []string `json:"supportedEfforts"`
	Effort         string   `json:"effort"`
	DefaultEffort  string   `json:"defaultEffort"`
}

func (e globalLooseEntry) info() ModelInfo {
	window := e.ContextWindow
	if window == 0 {
		window = e.MaxInputTokens
	}
	out := e.MaxOutputToken
	if out == 0 {
		out = e.MaxTokens
	}
	def := e.DefaultEffort
	if def == "" {
		def = e.Effort
	}
	return ModelInfo{
		ID:            firstNonEmpty(e.ID, e.ModelID, e.Model, e.Name),
		Name:          e.Name,
		ContextWindow: window,
		MaxTokens:     out,
		Efforts:       e.Efforts,
		DefaultEffort: def,
	}
}

// parseGlobalModelNames parses one enterprise probe answer.  It accepts the
// gateway envelope, a nested payload, a plain list and a bare id list, because
// the two paths genuinely differ.  credits are deliberately never parsed here:
// the global catalogue reports a credit field that does not mean what the CN one
// does.
func parseGlobalModelNames(raw []byte) ([]string, []ModelInfo, error) {
	var env struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Code != nil && *env.Code != 0 {
		return nil, nil, fmt.Errorf("global models code=%d", *env.Code)
	}
	payload := raw
	if t := strings.TrimSpace(string(env.Data)); t != "" && t != "null" {
		payload = env.Data
	}
	arr := resolveGlobalModelsArray(payload)
	if len(arr) == 0 {
		return nil, nil, errors.New("global models: no list found")
	}

	// Full entries first.
	var entries []dynModelEntry
	if err := json.Unmarshal(arr, &entries); err == nil && len(entries) > 0 {
		names := make([]string, 0, len(entries))
		infos := make([]ModelInfo, 0, len(entries))
		for _, e := range entries {
			if e.Disabled {
				continue
			}
			mi := e.modelInfo()
			if mi.ID == "" {
				mi.ID = firstNonEmpty(e.ID, e.ModelID, e.Model, e.Name)
			}
			if mi.ID == "" {
				continue
			}
			names = append(names, mi.ID)
			infos = append(infos, mi)
		}
		if len(names) > 0 {
			return names, infos, nil
		}
	}

	// Permissive entries.
	var loose []globalLooseEntry
	if err := json.Unmarshal(arr, &loose); err == nil && len(loose) > 0 {
		names := make([]string, 0, len(loose))
		infos := make([]ModelInfo, 0, len(loose))
		for _, e := range loose {
			if e.Disabled {
				continue
			}
			mi := e.info()
			if mi.ID == "" {
				continue
			}
			names = append(names, mi.ID)
			infos = append(infos, mi)
		}
		if len(names) > 0 {
			return names, infos, nil
		}
	}

	// A bare id list carries no metadata at all; infos stays nil rather than
	// inventing fields for models we know nothing about.
	var ids []string
	if err := json.Unmarshal(arr, &ids); err == nil && len(ids) > 0 {
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			if s := strings.TrimSpace(id); s != "" {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out, nil, nil
		}
	}
	return nil, nil, errors.New("global models empty list")
}

// resolveGlobalModelsArray digs the model array out of whichever wrapper the
// path used.
func resolveGlobalModelsArray(payload []byte) json.RawMessage {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		return json.RawMessage(trimmed)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return nil
	}
	for _, key := range []string{"models", "items", "list", "data", "result"} {
		if child, ok := obj[key]; ok {
			if found := resolveGlobalModelsArray(child); len(found) > 0 {
				return found
			}
		}
	}
	return nil
}

// extractEfforts splits a catalogue into ids, ids-and-metadata, and the two
// effort caches the rewrite pipeline reads.
func extractEfforts(infos []ModelInfo) (names []string, out []ModelInfo, efforts map[string][]string, defaults map[string]string) {
	names = make([]string, 0, len(infos))
	out = make([]ModelInfo, 0, len(infos))
	efforts = map[string][]string{}
	defaults = map[string]string{}
	for _, mi := range infos {
		if mi.ID == "" {
			continue
		}
		names = append(names, mi.ID)
		out = append(out, mi)
		if len(mi.Efforts) > 0 {
			efforts[mi.ID] = mi.Efforts
		}
		if mi.DefaultEffort != "" {
			defaults[mi.ID] = mi.DefaultEffort
		}
	}
	return names, out, efforts, defaults
}

// mergeGlobalCatalog unions two catalogues.  The primary keeps its order and
// wins on conflicts; the secondary only fills in ids the primary did not have.
// When the primary carried no metadata at all, the result carries none either.
func mergeGlobalCatalog(primaryNames []string, primaryInfos []ModelInfo, secondaryNames []string, secondaryInfos []ModelInfo) ([]string, []ModelInfo) {
	names := make([]string, 0, len(primaryNames)+len(secondaryNames))
	seen := map[string]bool{}
	for _, list := range [][]string{primaryNames, secondaryNames} {
		for _, n := range list {
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			names = append(names, n)
		}
	}
	if primaryInfos == nil {
		return names, nil
	}
	byID := map[string]ModelInfo{}
	order := make([]string, 0, len(primaryInfos)+len(secondaryInfos))
	for _, list := range [][]ModelInfo{primaryInfos, secondaryInfos} {
		for _, mi := range list {
			if mi.ID == "" {
				continue
			}
			if _, ok := byID[mi.ID]; ok {
				continue
			}
			byID[mi.ID] = mi
			order = append(order, mi.ID)
		}
	}
	infos := make([]ModelInfo, 0, len(order))
	for _, id := range order {
		infos = append(infos, byID[id])
	}
	return names, infos
}

// mergeEffortBuckets unions two effort maps; the primary is authoritative and
// the secondary fills only the keys it is missing.
func mergeEffortBuckets(primary, secondary map[string][]string) map[string][]string {
	out := make(map[string][]string, len(primary)+len(secondary))
	for k, v := range primary {
		out[k] = v
	}
	for k, v := range secondary {
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mergeEffortDefaults is mergeEffortBuckets for the default-effort map.
func mergeEffortDefaults(primary, secondary map[string]string) map[string]string {
	out := make(map[string]string, len(primary)+len(secondary))
	for k, v := range primary {
		out[k] = v
	}
	for k, v := range secondary {
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// dedupeNames removes duplicates while preserving order.
func dedupeNames(names []string) []string {
	out := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, n := range names {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// copyNames hands out a copy so a caller cannot mutate the cache.
func copyNames(names []string) []string {
	if names == nil {
		return nil
	}
	out := make([]string, len(names))
	copy(out, names)
	return out
}
