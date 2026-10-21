package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// The built-in catalogue.
//
// Two sources, neither of them sufficient alone:
//
//   - GET /models (public, no auth) is the authority on WHICH ids exist.  The
//     response is {"object":"list","data":[{"id":...,"object":"model",
//     "created":...,"owned_by":"opencode"}]} and it carries no display names
//     and no limits.  It returned 84 ids on 2026-10-01.
//   - The docs table (https://opencode.ai/docs/zen) and the embedded models.dev
//     registry entry for the `opencode` provider supply display names and, for
//     a minority of ids, limit:{context,output}.
//
// Merge order: the live list decides membership, the static table only fills in
// metadata.  An id the live list has but this table does not is still served,
// with a mechanically derived display name and NO limit — a fabricated context
// window is worse than an absent one.
//
// The limits matter beyond display: the gateway sizes its pre-send trim against
// the advertised context window.  A model with no window cannot be trimmed, so
// an over-long request is forwarded unchanged and the vendor refuses it with a
// 400 whose text names no number — leaving the guard unable to learn, and the
// caller without an answer.  Every id models.dev publishes a limit for is
// therefore carried here, free ids included.
// ---------------------------------------------------------------------------

// staticModel is one entry of the built-in catalogue.
type staticModel struct {
	id   string
	name string
	// context and output are the vendor-published limits, or 0 for "unknown".
	// They come from the models.dev registry entry embedded in the opencode
	// binary (see the README's provenance section); the live /models response
	// publishes none.
	context int
	output  int
	// deprecated is set for the ids the docs' "Deprecated models" table lists
	// AND that the live list still serves.
	deprecated bool
}

// staticCatalogue is ordered exactly as the live GET /models response returned
// the ids on 2026-10-01, so the fallback list a fresh install sees matches what
// the vendor serves.
var staticCatalogue = []staticModel{
	{id: "claude-fable-5", name: "Claude Fable 5", context: 1000000, output: 128000},
	{id: "claude-fable-5-1", name: "Claude Fable 5.1", context: 1000000, output: 128000},
	{id: "claude-opus-5-5", name: "Claude Opus 5.5", context: 1000000, output: 128000},
	{id: "claude-opus-5", name: "Claude Opus 5", context: 1000000, output: 128000},
	{id: "claude-opus-4-8", name: "Claude Opus 4.8", context: 1000000, output: 128000},
	{id: "claude-opus-4-7", name: "Claude Opus 4.7", context: 1000000, output: 128000},
	{id: "claude-opus-4-6", name: "Claude Opus 4.6", context: 1000000, output: 128000},
	{id: "claude-opus-4-5", name: "Claude Opus 4.5", context: 200000, output: 64000},
	{id: "claude-sonnet-5-5", name: "Claude Sonnet 5.5", context: 1000000, output: 128000},
	{id: "claude-sonnet-5", name: "Claude Sonnet 5", context: 1000000, output: 128000},
	{id: "claude-sonnet-4-6", name: "Claude Sonnet 4.6", context: 1000000, output: 64000},
	{id: "claude-sonnet-4-5", name: "Claude Sonnet 4.5", context: 1000000, output: 64000},
	{id: "claude-sonnet-4", name: "Claude Sonnet 4", context: 1000000, output: 64000, deprecated: true},
	{id: "claude-haiku-4-5", name: "Claude Haiku 4.5", context: 200000, output: 64000},
	{id: "gemini-3.6-flash", name: "Gemini 3.6 Flash", context: 1048576, output: 65536},
	{id: "gemini-3.8-flash", name: "Gemini 3.8 Flash", context: 1048576, output: 65536},
	{id: "gemini-3.7-flash", name: "Gemini 3.7 Flash", context: 1048576, output: 65536},
	{id: "gemini-3.5-flash-lite", name: "Gemini 3.5 Flash Lite", context: 1048576, output: 65536},
	{id: "gemini-3.5-flash", name: "Gemini 3.5 Flash", context: 1048576, output: 65536},
	{id: "gemini-3.1-pro", name: "Gemini 3.1 Pro", context: 1048576, output: 65536},
	{id: "gemini-3-flash", name: "Gemini 3 Flash", context: 1048576, output: 65536},
	{id: "gpt-6-astra", name: "GPT 6 Astra", context: 1050000, output: 128000},
	{id: "gpt-6.1-sol", name: "GPT 6.1 Sol", context: 1050000, output: 128000},
	{id: "gpt-6-sol", name: "GPT 6 Sol", context: 1050000, output: 128000},
	{id: "gpt-6-luna", name: "GPT 6 Luna", context: 1050000, output: 128000},
	{id: "gpt-5.6-sol", name: "GPT 5.6 Sol", context: 1050000, output: 128000},
	{id: "gpt-5.6-terra", name: "GPT 5.6 Terra", context: 1050000, output: 128000},
	{id: "gpt-5.6-luna", name: "GPT 5.6 Luna", context: 1050000, output: 128000},
	{id: "gpt-5.5", name: "GPT 5.5", context: 1050000, output: 128000},
	{id: "gpt-5.5-pro", name: "GPT 5.5 Pro", context: 1050000, output: 128000},
	{id: "gpt-5.4", name: "GPT 5.4", context: 1050000, output: 128000},
	{id: "gpt-5.4-pro", name: "GPT 5.4 Pro", context: 1050000, output: 128000},
	{id: "gpt-5.4-mini", name: "GPT 5.4 Mini", context: 400000, output: 128000},
	{id: "gpt-5.4-nano", name: "GPT 5.4 Nano", context: 400000, output: 128000},
	{id: "gpt-5.3-codex-spark", name: "GPT 5.3 Codex Spark", context: 128000, output: 128000},
	{id: "gpt-5.3-codex", name: "GPT 5.3 Codex", context: 400000, output: 128000},
	{id: "gpt-5.2", name: "GPT 5.2", context: 400000, output: 128000},
	{id: "gpt-5.2-codex", name: "GPT 5.2 Codex", context: 400000, output: 128000, deprecated: true},
	{id: "gpt-5.1", name: "GPT 5.1", context: 400000, output: 128000},
	{id: "gpt-5.1-codex-max", name: "GPT 5.1 Codex Max", context: 400000, output: 128000, deprecated: true},
	{id: "gpt-5.1-codex", name: "GPT 5.1 Codex", context: 400000, output: 128000, deprecated: true},
	{id: "gpt-5.1-codex-mini", name: "GPT 5.1 Codex Mini", context: 400000, output: 128000, deprecated: true},
	{id: "gpt-5", name: "GPT 5", context: 400000, output: 128000},
	{id: "gpt-5-codex", name: "GPT 5 Codex", context: 400000, output: 128000, deprecated: true},
	{id: "gpt-5-nano", name: "GPT 5 Nano", context: 400000, output: 128000},
	{id: "grok-build-0.1", name: "Grok Build 0.1", context: 256000, output: 256000},
	{id: "grok-4.7", name: "Grok 4.7", context: 500000, output: 500000},
	{id: "grok-4.6", name: "Grok 4.6", context: 500000, output: 500000},
	{id: "grok-4.5", name: "Grok 4.5", context: 500000, output: 500000},
	{id: "muse-spark-1.3", name: "Muse Spark 1.3", context: 1048576, output: 131072},
	{id: "muse-spark-1.2", name: "Muse Spark 1.2", context: 1048576, output: 131072},
	{id: "deepseek-v4.1-flash", name: "DeepSeek V4.1 Flash", context: 1000000, output: 384000},
	{id: "deepseek-v4-pro", name: "DeepSeek V4 Pro", context: 1000000, output: 384000},
	{id: "deepseek-v4-flash", name: "DeepSeek V4 Flash", context: 1000000, output: 384000},
	{id: "deepseek-v4-flash-vision-exp", name: "DeepSeek V4 Flash Vision Exp", context: 1000000, output: 384000},
	{id: "glm-5.3-flash", name: "GLM 5.3 Flash", context: 1000000, output: 131072},
	{id: "glm-5.3", name: "GLM 5.3", context: 1000000, output: 131072},
	{id: "glm-5.2", name: "GLM 5.2", context: 1000000, output: 131072},
	{id: "glm-5.1", name: "GLM 5.1", context: 204800, output: 131072},
	{id: "glm-5", name: "GLM 5", context: 204800, output: 131072, deprecated: true},
	{id: "minimax-m3", name: "MiniMax M3", context: 512000, output: 128000},
	{id: "minimax-m2.7", name: "MiniMax M2.7", context: 204800, output: 131072},
	{id: "minimax-m2.5", name: "MiniMax M2.5", context: 204800, output: 131072, deprecated: true},
	{id: "kimi-k3", name: "Kimi K3", context: 1048576, output: 131072},
	{id: "kimi-k2.7-code", name: "Kimi K2.7 Code", context: 262144, output: 262144},
	{id: "kimi-k2.6", name: "Kimi K2.6", context: 262144, output: 65536},
	{id: "kimi-k2.5", name: "Kimi K2.5", context: 262144, output: 65536, deprecated: true},
	{id: "qwen3.8-flash", name: "Qwen3.8 Flash", context: 1000000, output: 131072},
	{id: "qwen3.6-plus", name: "Qwen3.6 Plus", context: 262144, output: 65536},
	{id: "qwen3.5-plus", name: "Qwen3.5 Plus", context: 262144, output: 65536},
	{id: "jev-1.13", name: "Jev 1.13"},
	{id: "big-pickle", name: "Big Pickle", context: 200000, output: 32000},
	{id: "jev-1.13-free", name: "Jev 1.13 Free"},
	{id: "deepseek-v4-flash-free", name: "DeepSeek V4 Flash Free", context: 200000, output: 128000},
	{id: "muse-spark-1.3-contributor-free", name: "Muse Spark 1.3 Contributor Free", context: 1048576, output: 131072},
	{id: "muse-spark-1.2-contributor-free", name: "Muse Spark 1.2 Contributor Free", context: 1048576, output: 131072},
	{id: "mimo-v2.6-flash-free", name: "MiMo-V2.6-Flash Free", context: 200000, output: 32000},
	{id: "space-bunny-free", name: "Space Bunny Free", context: 1048576, output: 524288},
	{id: "longcat-2.5-preview-free", name: "LongCat 2.5 Preview Free", context: 1000000, output: 131072},
	{id: "mimo-v2.5-free", name: "MiMo-V2.5 Free", context: 200000, output: 32000},
	{id: "ling-3.0-flash-fin-free", name: "Ling 3.0 Flash Fin Free", context: 262144, output: 32768},
	{id: "nemotron-3-ultra-free", name: "Nemotron 3 Ultra Free", context: 1000000, output: 128000},
	{id: "nemotron-3.5-lightning-free", name: "Nemotron 3.5 Lightning Free", context: 262144, output: 262144},
	{id: "qwen3.8-max", name: "Qwen3.8 Max", context: 262144, output: 131072},
	{id: "beam-free", name: "Beam Free", context: 1048576, output: 131072},
	{id: "nemotron-3.5-super-vl-free", name: "Nemotron 3.5 Super VL Free", context: 1048576, output: 131072},
	{id: "deepseek-v4.1-flash-free", name: "DeepSeek V4.1 Flash Free", context: 200000, output: 32000},
	{id: "glm-5.3-flash-free", name: "GLM-5.3-Flash Free", context: 200000, output: 32000},
	{id: "minimax-m3.1-flash-free", name: "MiniMax M3.1 Flash Free", context: 1000000, output: 512000},
	{id: "claude-haiku-5-5", name: "Claude Haiku 5.5", context: 1000000, output: 128000},
	{id: "gemini-4-argon", name: "Gemini 4 Argon", context: 1048576, output: 65536},
	{id: "muse-wonder-1.0", name: "Muse Wonder", context: 1048576, output: 131072},
	{id: "mistral-large-4", name: "Mistral Large 4", context: 524288, output: 262144},
}

// staticIndex is the catalogue keyed by id, built once.
var staticIndex = buildStaticIndex(staticCatalogue)

func buildStaticIndex(cat []staticModel) map[string]staticModel {
	idx := make(map[string]staticModel, len(cat))
	for _, m := range cat {
		idx[m.id] = m
	}
	return idx
}

// staticModelIDs returns the built-in ids in catalogue order.
func staticModelIDs() []string {
	out := make([]string, 0, len(staticCatalogue))
	for _, m := range staticCatalogue {
		out = append(out, m.id)
	}
	return out
}

// displayNameFor returns the vendor's display name when the static table knows
// it, and otherwise derives one mechanically from the id.  A derived name is
// always marked so the panel can tell it apart from a published one.
func displayNameFor(id string) string {
	if m, ok := staticIndex[id]; ok && m.name != "" {
		return m.name
	}
	return deriveDisplayName(id)
}

// derivedNameSuffix marks a mechanically derived display name.
const derivedNameSuffix = " (derived)"

// vendorTokens maps the id fragments the catalogue uses to the brand spelling
// the vendor prints.  Anything absent is capitalised as-is, which is the best a
// mechanical rule can do — and why the result carries a marker.
var vendorTokens = map[string]string{
	"claude": "Claude", "gpt": "GPT", "gemini": "Gemini", "grok": "Grok",
	"deepseek": "DeepSeek", "glm": "GLM", "minimax": "MiniMax", "kimi": "Kimi",
	"qwen": "Qwen", "muse": "Muse", "mimo": "MiMo", "nemotron": "Nemotron",
	"longcat": "LongCat", "ling": "Ling", "jev": "Jev", "big": "Big",
	"pickle": "Pickle", "space": "Space", "bunny": "Bunny", "free": "Free",
	"contributor": "Contributor", "pro": "Pro", "mini": "Mini", "nano": "Nano",
	"codex": "Codex", "flash": "Flash", "plus": "Plus", "max": "Max",
	"ultra": "Ultra", "lightning": "Lightning", "preview": "Preview",
	"vision": "Vision", "exp": "Exp", "sol": "Sol", "terra": "Terra",
	"luna": "Luna", "astra": "Astra", "spark": "Spark", "opus": "Opus",
	"sonnet": "Sonnet", "haiku": "Haiku", "fable": "Fable", "build": "Build",
	"fin": "Fin", "code": "Code", "thinking": "Thinking", "coder": "Coder",
	"omni": "Omni", "turbo": "Turbo", "lite": "Lite",
}

// deriveDisplayName turns `muse-spark-1.2-contributor-free` into
// `Muse Spark 1.2 Contributor Free`.
func deriveDisplayName(id string) string {
	if strings.TrimSpace(id) == "" {
		return ""
	}
	parts := strings.Split(id, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		if tok, ok := vendorTokens[strings.ToLower(p)]; ok {
			parts[i] = tok
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ") + derivedNameSuffix
}

// newModel renders one catalogue entry as a core.Model.
func newModel(id, ownedBy string) core.Model {
	m := core.Model{ID: id, OwnedBy: ownedBy}
	extra := map[string]any{}
	if e, ok := staticIndex[id]; ok {
		if e.name != "" {
			extra["display_name"] = e.name
		}
		if e.context > 0 {
			extra["context_length"] = int64(e.context)
		}
		if e.output > 0 {
			extra["max_output_tokens"] = int64(e.output)
		}
		if e.deprecated {
			extra["deprecated"] = true
		}
	} else {
		extra["display_name"] = deriveDisplayName(id)
	}
	if len(extra) == 0 {
		return m
	}
	m.Extra = extra
	return m
}

// containsModel reports whether ids already holds id.
func containsModel(models []core.Model, id string) bool {
	for _, m := range models {
		if m.ID == id {
			return true
		}
	}
	return false
}

// freeModelSet is the allowlist of model ids the anonymous credential may
// ask for, as a set.
func freeModelSet(cfg Config) map[string]bool {
	ids := cfg.freeModels()
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" {
			out[id] = true
		}
	}
	return out
}

// isFreeModelID treats a vendor-published -free suffix as authoritative, with
// the configured allowlist covering free ids that do not carry the suffix.
func isFreeModelID(id string, allowed map[string]bool) bool {
	id = strings.TrimSpace(id)
	return id != "" && (allowed[id] || strings.HasSuffix(strings.ToLower(id), "-free"))
}

// anonymousModelAllowed reports whether an anonymous credential may ask for
// this model id.
func anonymousModelAllowed(id string, cfg Config) bool {
	return isFreeModelID(id, freeModelSet(cfg))
}

// probeModel is the model the panel's Test button runs for one account.  A
// configured test_model wins for a normal key, but an anonymous account can
// only use the free allowlist: probing a paid id would be refused with
// "Missing API key." and would falsely park an account that is fine.  When the
// configured model is itself free, it is kept.
func (c *Client) probeModel(a *accountRecord) string {
	configured := c.cfg.testModel()
	if a == nil || a.authMode() != "anonymous" {
		return configured
	}
	if anonymousModelAllowed(configured, c.cfg) {
		return configured
	}
	if ids := c.cfg.freeModels(); len(ids) > 0 {
		return strings.TrimSpace(ids[0])
	}
	return configured
}

// markFree tags a catalogue entry as free when its id carries the vendor's
// -free suffix or the configured allowlist covers it.
func markFree(m core.Model, allowed map[string]bool) core.Model {
	if !isFreeModelID(m.ID, allowed) {
		return m
	}
	if m.Extra == nil {
		m.Extra = map[string]any{}
	}
	m.Extra["free"] = true
	return m
}

// servedModels narrows and annotates the catalogue for the pool the module
// holds right now:
//
//   - an anonymous-only pool is narrowed to ids ending in -free plus the
//     configured allowlist, because those are what the public credential serves;
//   - matching entries are marked free:true wherever they appear;
//   - a signed-in workspace's allowed models are unioned in so the picker
//     offers the models that account can actually use.
//
// It builds a fresh slice and never mutates its input, so a cached list stays
// untouched across pool changes.
func (c *Client) servedModels(in []core.Model) []core.Model {
	allowed := freeModelSet(c.cfg)
	anonymousOnly := c.pool.onlyAnonymous()
	seen := make(map[string]bool, len(in))
	out := make([]core.Model, 0, len(in))
	for _, m := range in {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		free := isFreeModelID(m.ID, allowed)
		if c.cfg.freeOnly() && !free {
			continue
		}
		if anonymousOnly && !free {
			continue
		}
		seen[m.ID] = true
		cp := m
		if m.Extra != nil {
			extra := make(map[string]any, len(m.Extra)+1)
			for k, v := range m.Extra {
				extra[k] = v
			}
			cp.Extra = extra
		}
		if free {
			if cp.Extra == nil {
				cp.Extra = map[string]any{}
			}
			cp.Extra["free"] = true
		}
		out = append(out, cp)
	}
	if !anonymousOnly {
		for _, a := range c.pool.snapshot() {
			if a.authMode() != "oauth" {
				continue
			}
			for _, id := range a.AllowedModels {
				id = strings.TrimSpace(id)
				if id == "" || seen[id] {
					continue
				}
				if c.cfg.freeOnly() && !isFreeModelID(id, allowed) {
					continue
				}
				seen[id] = true
				out = append(out, newModel(id, clientName))
			}
		}
	}
	return out
}

// fallbackModels is the built-in catalogue plus any configured extras.  It is
// what a fresh install — and an outage — answers with.
func fallbackModels(cfg Config) []core.Model {
	ids := staticModelIDs()
	allowed := freeModelSet(cfg)
	out := make([]core.Model, 0, len(ids)+len(cfg.ExtraModels))
	for _, id := range ids {
		out = append(out, markFree(newModel(id, clientName), allowed))
	}
	for _, id := range cfg.ExtraModels {
		if containsModel(out, id) {
			continue
		}
		out = append(out, markFree(newModel(id, clientName), allowed))
	}
	return out
}

// mergeCatalogue builds the served list from the live id list.  Membership
// comes from live; names and limits come from the static table.
func mergeCatalogue(liveIDs []string, cfg Config) []core.Model {
	out := make([]core.Model, 0, len(liveIDs)+len(cfg.ExtraModels))
	allowed := freeModelSet(cfg)
	seen := make(map[string]bool, len(liveIDs))
	for _, id := range liveIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, markFree(newModel(id, clientName), allowed))
	}
	for _, id := range cfg.ExtraModels {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, markFree(newModel(id, clientName), allowed))
	}
	return out
}

// modelListResponse is the GET /models envelope.
type modelListResponse struct {
	Object string `json:"object"`
	Data   []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
		// modelId is tolerated because other vendors in this repo spell it that
		// way and a mirror may follow them.
		ModelID string `json:"modelId"`
	} `json:"data"`
}

// parseModelList extracts the ids from a GET /models body.
func parseModelList(raw []byte) ([]string, error) {
	var resp modelListResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%s: decoding model list: %w", clientName, err)
	}
	ids := make([]string, 0, len(resp.Data))
	for _, m := range resp.Data {
		id := strings.TrimSpace(firstNonEmpty(m.ID, m.ModelID))
		if id == "" {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%s: model list carried no ids", clientName)
	}
	return ids, nil
}

// fetchModels is the only network path that reads the catalogue.  It is never
// reached from ModelMaxOutputTokens or from Status.
func (c *Client) fetchModels(ctx context.Context) ([]core.Model, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.modelsTimeout())
	defer cancel()

	spec := requestSpec{
		method: http.MethodGet,
		url:    c.cfg.modelsURL(),
		accept: "application/json",
	}
	// GET /models is public.  Sending no credential keeps the module from
	// leaking a key to an endpoint that does not need one.
	resp, err := c.do(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("%s: listing models: %w", clientName, err)
	}
	defer resp.Body.Close()

	body := readLimited(resp.Body, 4<<20)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: listing models: HTTP %d: %s",
			clientName, resp.StatusCode, truncate(core.Redact(string(body)), 200))
	}
	ids, err := parseModelList(body)
	if err != nil {
		return nil, err
	}
	return mergeCatalogue(ids, c.cfg), nil
}

// ---------------------------------------------------------------------------
// Cache accessors.
// ---------------------------------------------------------------------------

// cachedModels returns the live list when it is younger than models_ttl.
func (c *Client) cachedModels() ([]core.Model, bool) {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	if len(c.models) == 0 || c.modelsAt.IsZero() {
		return nil, false
	}
	if c.now().Sub(c.modelsAt) >= c.cfg.modelsTTL() {
		return nil, false
	}
	return copyModels(c.models), true
}

// lastModels returns the last good list at any age.  A failed refresh falls
// back to it so an outage never empties the model picker.
func (c *Client) lastModels() ([]core.Model, bool) {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	if len(c.models) == 0 {
		return nil, false
	}
	return copyModels(c.models), true
}

// storeModels records a freshly fetched list.
func (c *Client) storeModels(models []core.Model) {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	c.models = copyModels(models)
	c.modelsAt = c.now()
}

// modelIDs renders the served ids for Status.  Status must never block or make
// a network call, so this reads the cache and otherwise falls back.
func (c *Client) modelIDs() []string {
	models, ok := c.lastModels()
	if !ok {
		models = fallbackModels(c.cfg)
	}
	models = c.servedModels(models)
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids
}

// ---------------------------------------------------------------------------
// core.ModelLimitsProvider.
//
// The contract for this interface is unusually strict: it is consulted on every
// request that omitted max_tokens, so it must be cheap, side-effect free and
// must never fetch anything.  This module satisfies it from the in-memory
// static table only, and answers ok=false for the ids the table does not know.
// ---------------------------------------------------------------------------

// ModelMaxOutputTokens reports the vendor-published output budget for an
// upstream model id.
//
// ok is false for the 73 of 84 live ids whose limit the vendor does not
// publish anywhere this module can read.  It declines rather than guessing: a
// fabricated cap silently truncates a caller's answer.
func (c *Client) ModelMaxOutputTokens(ctx context.Context, model string) (int, bool) {
	id := strings.TrimSpace(model)
	if id == "" {
		return 0, false
	}
	if e, ok := staticIndex[id]; ok && e.output > 0 {
		return e.output, true
	}
	// The live list carries no limits, but honour one if a future response
	// grows a max_output_tokens field — read from cache, never fetched.
	if models, ok := c.lastModels(); ok {
		if n, ok := core.OutputLimitFor(models, id); ok {
			return n, true
		}
	}
	return 0, false
}
