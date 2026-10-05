package zcode

// The upstream model catalogue.
//
// The vendor really does serve GET {base}/v1/models, but only to an
// authenticated caller: with no credential the endpoint answers HTTP 200 with
//
//	{"code":1001,"msg":"Authentication parameter not received in Header,
//	 unable to authenticate","success":false}
//
// rather than a 401.  That envelope is a FAILURE and is treated as one here; it
// must never be mistaken for a successful empty list, or the panel would say
// "no models" where the truth is "your credential is expired".
//
// Models() is called on every panel refresh, so it answers from a TTL cache and
// only reaches upstream when that cache has gone stale.  Nothing in this file
// may empty the catalogue: every path falls back to the last good list and then
// to the configured one.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

const (
	// modelsPath is appended to an account's chat base.  Reusing the chat base
	// is deliberate: an account's base_url override has to point the model list
	// at the same endpoint family its chat requests go to, and only that
	// account's own credential is ever sent there.
	modelsPath = "/v1/models"

	// modelsCacheTTL is how long one upstream answer -- or one failed attempt --
	// is trusted.  Inside the window Models() never touches the network.
	// Caching the failure matters as much as caching the answer: without it a
	// vendor that is down would be re-dialled on every panel refresh.
	modelsCacheTTL = 5 * time.Minute

	// modelsFetchTimeout bounds one /v1/models request.  It is deliberately far
	// shorter than Config.TimeoutSeconds (600s): this is a small metadata call
	// on a path a human is waiting for, not a chat completion.
	modelsFetchTimeout = 5 * time.Second

	// maxModelsBodyBytes caps how much of the response body is read.  A model
	// list is a few kilobytes; anything past this is not a model list.
	maxModelsBodyBytes = 1 << 20

	// modelsDetailLimit bounds a vendor message embedded in an error string.
	modelsDetailLimit = 200
)

// errNoModelAccount means the pool holds nothing that could ask the vendor.
// It is a local condition, not a network failure, so it must not start a TTL
// window: the operator may add a credential a second from now and the very
// next Models() call should be free to try it.
var errNoModelAccount = errors.New("zcode: no usable account to fetch the model list from")

// ---------------------------------------------------------------------------
// Cache
// ---------------------------------------------------------------------------

// modelCache is the module's in-memory copy of the vendor's catalogue.  Every
// field is guarded by mu and the zero value is ready to use.
type modelCache struct {
	mu sync.Mutex

	// list is the last upstream answer that carried at least one model.  It
	// stays nil until one succeeds, so it can never blank the catalogue.
	list []core.Model
	// storedAt is when list was stored.
	storedAt time.Time
	// attemptedAt is when upstream was last asked, successfully or not.
	attemptedAt time.Time

	// inflight is the refresh one goroutine is running right now.  A burst of
	// concurrent callers therefore becomes ONE vendor request whose outcome
	// the others reuse.
	inflight *modelRefresh
}

// modelRefresh is one in-flight upstream attempt.  Its fields are written by
// the goroutine that claimed the refresh and read by the joiners only after
// done is closed, which is what makes the hand-off race-free: every joiner
// owns its own view rather than reading a field a later refresh may reset.
type modelRefresh struct {
	done chan struct{}
	list []core.Model
	err  error
}

func newModelCache() *modelCache { return &modelCache{} }

// cached returns the last good list while it is still fresh.
func (mc *modelCache) cached(now time.Time, ttl time.Duration) ([]core.Model, bool) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if len(mc.list) == 0 || mc.storedAt.IsZero() || now.Sub(mc.storedAt) >= ttl {
		return nil, false
	}
	return cloneModels(mc.list), true
}

// snapshot returns the last good list whatever its age, or nil when there has
// never been one.
func (mc *modelCache) snapshot() []core.Model {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	return cloneModels(mc.list)
}

// triedRecently reports whether upstream was already asked inside ttl, whether
// or not that attempt produced a list.
func (mc *modelCache) triedRecently(now time.Time, ttl time.Duration) bool {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	return !mc.attemptedAt.IsZero() && now.Sub(mc.attemptedAt) < ttl
}

// beginRefresh claims the right to talk to upstream.  The claimant gets true
// and must call finishRefresh; everyone else gets the in-flight refresh to wait
// on, so concurrent refreshes collapse into one request.
func (mc *modelCache) beginRefresh() (*modelRefresh, bool) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if mc.inflight != nil {
		return mc.inflight, false
	}
	ref := &modelRefresh{done: make(chan struct{})}
	mc.inflight = ref
	return ref, true
}

// finishRefresh publishes the outcome and releases the joiners.  A zero
// attempted time leaves the TTL window alone: a purely local condition ("no
// account to ask") is not an upstream attempt and must not suppress the next
// one.  A refresh that carries no list never clears the cached one, which is
// what keeps a failed refresh from emptying the catalogue.
func (mc *modelCache) finishRefresh(ref *modelRefresh, list []core.Model, err error, attempted time.Time) {
	mc.mu.Lock()
	if len(list) > 0 {
		mc.list = cloneModels(list)
		mc.storedAt = time.Now()
	}
	if !attempted.IsZero() {
		mc.attemptedAt = attempted
	}
	mc.inflight = nil
	mc.mu.Unlock()

	ref.list = cloneModels(list)
	ref.err = err
	close(ref.done)
}

// cloneModels copies the slice so a caller can never reach into the cache
// through the value it was handed.
func cloneModels(list []core.Model) []core.Model {
	if len(list) == 0 {
		return nil
	}
	out := make([]core.Model, len(list))
	copy(out, list)
	return out
}

// ---------------------------------------------------------------------------
// core.Client / core.ModelRefresher
// ---------------------------------------------------------------------------

// Models reports the model ids this module can route.
//
// It is cheap by construction: it answers from the TTL cache and only asks
// upstream when that cache has gone stale, so a panel refresh is not a vendor
// request.  It never fails and never returns an empty catalogue -- when
// upstream cannot be reached, or no usable credential exists, the last good
// list (or, before there ever was one, the configured list) is returned.
func (c *Client) Models(ctx context.Context) ([]core.Model, error) {
	now := time.Now()
	if list, ok := c.models.cached(now, modelsCacheTTL); ok {
		return list, nil
	}
	if !c.models.triedRecently(now, modelsCacheTTL) {
		if list, err := c.refreshModels(ctx); err == nil {
			return list, nil
		}
	}
	return c.catalogFallback(), nil
}

// RefreshModels implements core.ModelRefresher: it bypasses the TTL and asks
// the vendor again.
//
// A failed refresh NEVER empties the catalogue.  It returns the last good list
// -- or, when there has never been one, the configured list -- alongside the
// error, so one flaky refresh cannot blank the panel's model picker.
func (c *Client) RefreshModels(ctx context.Context) ([]core.Model, error) {
	list, err := c.refreshModels(ctx)
	if err != nil {
		return c.catalogFallback(), err
	}
	return list, nil
}

// catalogFallback is what the module reports when upstream has nothing to say:
// the last good list, else the configured ids.
func (c *Client) catalogFallback() []core.Model {
	if list := c.models.snapshot(); len(list) > 0 {
		return list
	}
	return c.configuredModels()
}

// configuredModels projects the configured model ids onto core.Model.  It is
// the pre-upstream behaviour, kept as the floor the catalogue can never fall
// below.
func (c *Client) configuredModels() []core.Model {
	ids := c.cfg.modelIDs()
	out := make([]core.Model, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		out = append(out, applyBuiltinSpec(core.Model{ID: id, OwnedBy: c.Name()}))
	}
	return out
}

// ModelMaxOutputTokens implements core.ModelLimitsProvider.  When the caller
// omits max_tokens the gateway asks what the vendor advertises for this model,
// rather than letting the module's flat defaultMaxTokens decide for a model
// that advertises far more.
//
// The vendor's /v1/models list carries no budget at all, so every catalogue
// entry is filled from builtinModelSpecs on the way in (see modelFromEntry).
// The same table also answers before the first upstream refresh, which matters
// on a cold start: a known plan model must not be truncated just because the
// metadata cache has not warmed up yet.  An operator-typed id the table does
// not know still answers "cannot say"; it never gets an invented budget.
func (c *Client) ModelMaxOutputTokens(ctx context.Context, model string) (int, bool) {
	if n, ok := core.OutputLimitFor(c.models.snapshot(), model); ok {
		return n, true
	}
	spec, ok := builtinModelSpecs[strings.ToLower(strings.TrimSpace(model))]
	if !ok || spec.maxOutputTokens <= 0 {
		return 0, false
	}
	return spec.maxOutputTokens, true
}

// refreshModels performs one upstream refresh and updates the cache.  A burst
// of concurrent callers collapses into a single vendor request: one goroutine
// does the work and the rest wait for it and reuse its outcome.
func (c *Client) refreshModels(ctx context.Context) ([]core.Model, error) {
	ref, claimed := c.models.beginRefresh()
	if !claimed {
		select {
		case <-ref.done:
		case <-ctx.Done():
			return nil, fmt.Errorf("zcode: model list refresh cancelled: %w", ctx.Err())
		}
		return cloneModels(ref.list), ref.err
	}

	now := time.Now()
	list, err := c.fetchModels(ctx)
	switch {
	case err != nil:
		// A real upstream attempt was made, so hold the next one off for a TTL
		// and do not re-dial a dead vendor on every panel refresh.  A local
		// "no account" answer is not an attempt and starts no window.
		attempted := now
		if errors.Is(err, errNoModelAccount) {
			attempted = time.Time{}
		}
		c.models.finishRefresh(ref, nil, err, attempted)
		return nil, err
	case len(list) == 0:
		empty := errors.New("zcode: the vendor returned an empty model list")
		c.models.finishRefresh(ref, nil, empty, now)
		return nil, empty
	}
	c.models.finishRefresh(ref, list, nil, now)
	return list, nil
}

// fetchModels tries every credential the pool could use, in the pool's own
// order, and stops at the first that answers with a real list.  A credential
// that fails is skipped rather than fatal: one expired key must not hide the
// catalogue from a working one.
func (c *Client) fetchModels(ctx context.Context) ([]core.Model, error) {
	accounts := c.modelAccounts()
	if len(accounts) == 0 {
		return nil, errNoModelAccount
	}

	var lastErr error
	for _, acct := range accounts {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("zcode: model list refresh cancelled: %w", err)
		}
		list, err := c.fetchModelsFrom(ctx, acct)
		if err == nil {
			return list, nil
		}
		lastErr = err
		c.deps.Log("zcode: model list from account %q failed: %v", acct.ID, err)
	}
	if lastErr == nil {
		return nil, errNoModelAccount
	}
	return nil, lastErr
}

// modelAccounts is the ordered list of credentials the pool would use right
// now, plus the upstream_base stopgap when the operator pinned one endpoint.
func (c *Client) modelAccounts() []*Account {
	if strings.TrimSpace(c.cfg.UpstreamBase) != "" {
		return []*Account{stopgapAccount(c.cfg)}
	}
	return c.pool.selectableAccounts()
}

// fetchModelsFrom performs one GET {base}/v1/models, signed exactly the way
// this account signs a chat request.
func (c *Client) fetchModelsFrom(ctx context.Context, acct *Account) ([]core.Model, error) {
	url := c.modelsURL(acct)
	if url == "" {
		return nil, errors.New("zcode: no base URL is configured for the model list")
	}

	reqCtx, cancel := context.WithTimeout(ctx, modelsFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		// The URL is deliberately not echoed: an operator's base_url could
		// carry a credential in a query string.
		return nil, errors.New("zcode: cannot build the model list request")
	}
	// The same identity, auth and trace headers as a chat request: a model
	// list signed differently from the traffic it describes is worthless.
	// The captcha headers are deliberately absent -- a model list is not a
	// chat turn, and solving the captcha here would burn a solver run per
	// catalogue refresh.
	c.applyHeaders(req, acct, "", "")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New(scrubSecrets("zcode: model list request failed: "+err.Error(), acct.secret()))
	}
	defer func() { _ = resp.Body.Close() }()

	raw := readLimited(resp.Body, maxModelsBodyBytes)

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, c.modelListRefusal(acct, resp.StatusCode, raw)
	}
	list, err := parseModelList(raw, c.Name())
	if err != nil {
		return nil, errors.New(scrubSecrets(err.Error(), acct.secret()))
	}
	return list, nil
}

// modelsURL turns an account's chat endpoint into its model-list endpoint,
// reusing the same base-URL resolver the chat path uses.
func (c *Client) modelsURL(acct *Account) string {
	base := strings.TrimSuffix(c.upstreamURL(acct), messagesPath)
	return joinModels(base)
}

// joinModels builds the model-list URL for a base that may or may not already
// carry the path.
func joinModels(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	base = strings.TrimRight(base, "/")
	switch {
	case strings.HasSuffix(base, modelsPath):
		return base
	case strings.HasSuffix(base, "/v1"):
		return base + "/models"
	}
	return base + modelsPath
}

// modelListRefusal renders a non-2xx model-list response without ever echoing
// a credential.
func (c *Client) modelListRefusal(acct *Account, status int, raw []byte) error {
	text := fmt.Sprintf("zcode: the vendor refused the model list (HTTP %d)", status)
	if detail := modelErrorDetail(raw); detail != "" {
		text += ": " + truncate(detail, modelsDetailLimit)
	}
	return errors.New(scrubSecrets(text, acct.secret()))
}

// ---------------------------------------------------------------------------
// Response decoding
// ---------------------------------------------------------------------------

// modelListEnvelope is the union of the shapes the vendor is known to answer
// /v1/models with.  Code is `any` because it arrives as a number on one host
// and as a string on another, and Data is raw because it is sometimes a bare
// array and sometimes an object wrapping one.
type modelListEnvelope struct {
	Code    any             `json:"code"`
	Success *bool           `json:"success"`
	Msg     string          `json:"msg"`
	Message string          `json:"message"`
	Error   json.RawMessage `json:"error"`
	Data    json.RawMessage `json:"data"`
	Models  []modelEntry    `json:"models"`
}

// modelEntry is one model record.  Only fields the vendor actually sends are
// read; an absent field stays absent and is never invented.
type modelEntry struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	DisplayName     string `json:"display_name"`
	ContextLength   any    `json:"context_length"`
	ContextWindow   any    `json:"context_window"`
	MaxOutputTokens any    `json:"max_output_tokens"`
	MaxTokens       any    `json:"max_tokens"`
}

// parseModelList decodes a model-list response.
//
// It accepts the Anthropic shape ({"data":[{"id":...}]}), the z.ai envelope
// ({"code":0,"msg":"","success":true,"data":[...]}) and the OpenAI-ish
// {"data":[...]} / {"models":[...]} pair, including an object-wrapped data
// member.  A non-zero code, success:false or an "error" member is reported as
// a FAILURE even though it arrives with HTTP 200.
func parseModelList(raw []byte, ownedBy string) ([]core.Model, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("zcode: the vendor returned an empty model list response")
	}

	var env modelListEnvelope
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return nil, fmt.Errorf("zcode: the model list response is not JSON: %w", err)
	}

	entries := decodeModelEntries(env.Data)
	if len(entries) == 0 {
		entries = env.Models
	}
	if err := modelListFailure(env, len(entries) > 0); err != nil {
		return nil, err
	}

	out := make([]core.Model, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		m, ok := modelFromEntry(e, ownedBy)
		if !ok || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, errors.New("zcode: the vendor returned no models")
	}
	return out, nil
}

// modelListFailure reports the vendor's own refusal, which is what a non-zero
// code, success:false or an "error" member means.
func modelListFailure(env modelListEnvelope, hasEntries bool) error {
	if code, bad := failureCode(env.Code, hasEntries); bad {
		return fmt.Errorf("zcode: the vendor rejected the model list (code %s): %s",
			code, modelDetailOr(env, "no message"))
	}
	if env.Success != nil && !*env.Success {
		return fmt.Errorf("zcode: the vendor rejected the model list: %s", modelDetailOr(env, "no message"))
	}
	if detail := errorObjectMessage(env.Error); detail != "" {
		return fmt.Errorf("zcode: the vendor rejected the model list: %s", truncate(detail, modelsDetailLimit))
	}
	return nil
}

// failureCode decides whether a vendor code is a refusal.  A "200" is tolerated
// as a success code when the body really does carry models, because some hosts
// answer an OpenAI-style {"code":200,...} envelope instead of z.ai's {"code":0}.
func failureCode(v any, hasEntries bool) (string, bool) {
	text := codeText(v)
	switch {
	case text == "", text == "0":
		return "", false
	case text == "200" && hasEntries:
		return "", false
	}
	return text, true
}

// codeText renders a vendor code that may have arrived as a number or a string.
func codeText(v any) string {
	switch n := v.(type) {
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	case json.Number:
		return n.String()
	case string:
		return strings.TrimSpace(n)
	}
	return ""
}

// decodeModelEntries accepts the several shapes the "data" member takes: a bare
// array, or an object wrapping the array under "models" or "data".
func decodeModelEntries(raw json.RawMessage) []modelEntry {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var list []modelEntry
	if err := json.Unmarshal(raw, &list); err == nil {
		return list
	}
	var wrapped struct {
		Models []modelEntry `json:"models"`
		Data   []modelEntry `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil {
		if len(wrapped.Models) > 0 {
			return wrapped.Models
		}
		return wrapped.Data
	}
	return nil
}

// builtinModelSpec is what the vendor publishes for one model id.
//
// The ZCode plan's /v1/models list answers with ids and display names and
// nothing else, so without this table every model reached the gateway with no
// advertised budget and fell back to the module's flat defaultMaxTokens.
// A caller that omitted max_tokens was then cut off mid-answer with
// finish_reason "length" and nothing in the response naming who chose the
// number.  The values below are the ones docs.z.ai publishes for each id.
type builtinModelSpec struct {
	displayName     string
	contextLength   int
	maxOutputTokens int
}

// builtinModelSpecs is keyed by the upstream model id in lower case.  It is a
// fallback, never an override: a number the vendor sent always wins.
var builtinModelSpecs = map[string]builtinModelSpec{
	"glm-4.5":        {"GLM-4.5", 131072, 98304},
	"glm-4.5-air":    {"GLM-4.5-Air", 131072, 98304},
	"glm-4.6":        {"GLM-4.6", 204800, 131072},
	"glm-4.7":        {"GLM-4.7", 204800, 131072},
	"glm-5":          {"GLM-5", 204800, 131072},
	"glm-5-turbo":    {"GLM-5-Turbo", 202752, 131072},
	"glm-5.1":        {"GLM-5.1", 204800, 131072},
	"glm-5.2":        {"GLM-5.2", 1048576, 131072},
	"glm-5.3":        {"GLM-5.3", 1048576, 131072},
	"glm-5.3-flash":  {"GLM-5.3-Flash", 1048576, 131072},
	"glm-5.3-flashx": {"GLM-5.3-FlashX", 1048576, 131072},
}

// applyBuiltinSpec fills the gaps a vendor record left open.  A value the
// vendor actually published is never overwritten.
func applyBuiltinSpec(m core.Model) core.Model {
	spec, ok := builtinModelSpecs[strings.ToLower(strings.TrimSpace(m.ID))]
	if !ok {
		return m
	}
	if m.Extra == nil {
		m.Extra = make(map[string]any, 3)
	}
	if _, ok := m.Extra["display_name"]; !ok {
		m.Extra["display_name"] = spec.displayName
	}
	if _, ok := m.Extra["context_length"]; !ok {
		m.Extra["context_length"] = spec.contextLength
	}
	if _, ok := m.Extra["max_output_tokens"]; !ok {
		m.Extra["max_output_tokens"] = spec.maxOutputTokens
	}
	return m
}

// modelFromEntry projects one vendor record.  Values the vendor did not send
// are not invented: Extra stays nil unless the record actually carried one.
func modelFromEntry(e modelEntry, ownedBy string) (core.Model, bool) {
	id := strings.TrimSpace(e.ID)
	if id == "" {
		return core.Model{}, false
	}
	m := core.Model{ID: id, OwnedBy: ownedBy}

	var extra map[string]any
	set := func(key string, value any) {
		if extra == nil {
			extra = make(map[string]any, 3)
		}
		extra[key] = value
	}
	if name := firstNonEmpty(e.DisplayName, e.Name); name != "" && name != id {
		set("display_name", name)
	}
	if n, ok := toInt64(firstNonNil(e.ContextLength, e.ContextWindow)); ok && n > 0 {
		set("context_length", n)
	}
	if n, ok := toInt64(firstNonNil(e.MaxOutputTokens, e.MaxTokens)); ok && n > 0 {
		set("max_output_tokens", n)
	}
	m.Extra = extra
	return applyBuiltinSpec(m), true
}

// toInt64 coerces a numeric field the vendor may have sent as a number or as a
// string.  A value that is not a number is dropped rather than guessed at.
func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
		if f, err := n.Float64(); err == nil {
			return int64(f), true
		}
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i, true
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int64(f), true
		}
	}
	return 0, false
}

// firstNonNil returns the first value that was actually present.
func firstNonNil(vals ...any) any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

// modelErrorDetail pulls a short vendor message out of a refusal body.
func modelErrorDetail(raw []byte) string {
	var env modelListEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(raw), &env); err != nil {
		return ""
	}
	if detail := firstNonEmpty(env.Msg, env.Message); detail != "" {
		return detail
	}
	return errorObjectMessage(env.Error)
}

// modelDetailOr is modelErrorDetail with a fallback for the error text.
func modelDetailOr(env modelListEnvelope, fallback string) string {
	if detail := firstNonEmpty(env.Msg, env.Message); detail != "" {
		return truncate(detail, modelsDetailLimit)
	}
	if detail := errorObjectMessage(env.Error); detail != "" {
		return truncate(detail, modelsDetailLimit)
	}
	return fallback
}

// errorObjectMessage reads the message out of an "error" member, whether the
// vendor sent an object or a bare string.
func errorObjectMessage(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	var obj struct {
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	if err := json.Unmarshal(trimmed, &obj); err == nil {
		if detail := firstNonEmpty(obj.Message, obj.Msg); detail != "" {
			return strings.TrimSpace(detail)
		}
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return strings.TrimSpace(text)
	}
	return ""
}
