package lobsterai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"client2api/internal/core"
)

// staticContextLength is the context window reported for a model whose entry
// carries no usable `contextWindow`.
//
// It is a bridge-layer estimate, NOT a vendor-published number, and it is now
// only the FALLBACK.  The live /api/models/available response does publish a
// per-model `contextWindow` (measured 2026-10-01: 1000000 for deepseek-flash,
// glm-5.3 and the qwen3.8 family; 262144 for the kimi-k2.x family; 256000 for
// doubao-seed-2-1), but nine rows -- MiniMax-M2.7, qwen3.6-plus,
// qwen3.5-plus-2026-04-20, kimi-k2.6, kimi-k2.5, glm-5.1, glm-5v-turbo, glm-5
// and doubao-seed-2-0-code-preview-260215 -- send it as null.  Those keep this
// number.
const staticContextLength = 131072

// staticModelIDs is the fallback catalogue, in the order the vendor lists them.
// It is used before the first successful fetch, and whenever the fetch fails, so
// that routing and the panel stay useful while the upstream is unreachable.
var staticModelIDs = []string{
	"deepseek-v4-flash",
	"deepseek-v4-pro",
	"qwen3.7-max",
	"qwen3.7-plus",
	"qwen3.6-plus",
	"qwen3.5-plus-2026-04-20",
	"kimi-k2.7-code",
	"kimi-k2.7-code-highspeed",
	"kimi-k2.6",
	"kimi-k2.5",
	"doubao-seed-2-1-pro-260628",
	"doubao-seed-2-1-turbo-260628",
	"doubao-seed-2-0-code-preview-260215",
	"glm-5.2",
	"glm-5.1",
	"glm-5v-turbo",
	"glm-5",
	"MiniMax-M3",
	"MiniMax-M2.7",
}

// fallbackModels renders the built-in catalogue plus anything the operator
// configured by hand.
func fallbackModels(cfg Config) []core.Model {
	out := make([]core.Model, 0, len(staticModelIDs)+len(cfg.ExtraModels))
	for _, id := range staticModelIDs {
		out = append(out, newModel(id, "lobsterai", staticContextLength, ""))
	}
	for _, id := range cfg.ExtraModels {
		if id == "" || containsModel(out, id) {
			continue
		}
		out = append(out, newModel(id, "lobsterai", staticContextLength, ""))
	}
	return out
}

// newModel builds one catalogue entry.
//
// `contextLength` is the vendor's published window when the entry carried one,
// and staticContextLength when it did not.  max_output_tokens is deliberately
// absent because the vendor publishes no output budget anywhere in
// /api/models/available -- see the README for why this module therefore does not
// implement core.ModelLimitsProvider.
func newModel(id, ownedBy string, contextLength int64, displayName string) core.Model {
	extra := map[string]any{
		"context_length": contextLength,
	}
	// The panel prefers display_name over the raw id, and the vendor does
	// publish a human-readable modelName ("DeepSeek-V4.1-Flash"); only set it
	// when there is one, so a model with no name is not given an empty label.
	if displayName != "" {
		extra["display_name"] = displayName
	}
	return core.Model{
		ID:      id,
		OwnedBy: firstNonEmpty(ownedBy, "lobsterai"),
		Extra:   extra,
	}
}

func containsModel(models []core.Model, id string) bool {
	for _, m := range models {
		if m.ID == id {
			return true
		}
	}
	return false
}

// modelEntry is one element of the /api/models/available data array.  The
// "id" spelling is tolerated as well because the endpoint is OpenAI-shaped and
// gateways in front of it sometimes rewrite the key.
//
// ContextWindow is decoded as any rather than int64 because the vendor has been
// seen to type it as a number OR as null (nine of the 29 live rows send null);
// toInt covers both, and a null simply leaves the fallback in place.
type modelEntry struct {
	ModelID       string `json:"modelId"`
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	ModelName     string `json:"modelName"`
	ContextWindow any    `json:"contextWindow"`
}

// parseModelList projects the upstream data array into core models.
func parseModelList(raw json.RawMessage) ([]core.Model, error) {
	var entries []modelEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("models: unexpected payload: %w", err)
	}
	out := make([]core.Model, 0, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(firstNonEmpty(entry.ModelID, entry.ID))
		if id == "" || containsModel(out, id) {
			continue
		}
		contextLength := int64(staticContextLength)
		if n, ok := toInt(entry.ContextWindow); ok && n > 0 {
			contextLength = int64(n)
		}
		out = append(out, newModel(id, entry.Provider, contextLength, strings.TrimSpace(entry.ModelName)))
	}
	return out, nil
}

// fetchModels asks the upstream for the live list.
//
// It is a network call, so it is never reached from ModelMaxOutputTokens or
// Status; Models/RefreshModels call it behind a TTL.
func (c *Client) fetchModels(ctx context.Context) ([]core.Model, error) {
	acct := c.pool.firstReady(c.now(), c.cfg.maxInFlight())
	if acct == nil {
		return nil, fmt.Errorf("%w: no LobsterAI account is available to list models", core.ErrNotConfigured)
	}
	if err := c.ensureFresh(ctx, acct); err != nil {
		return nil, err
	}
	version := c.clientVersion(ctx)
	spec := requestSpec{
		method:  http.MethodGet,
		url:     c.cfg.modelsURL(acct.accountConfig(), version),
		bearer:  acct.AccessToken,
		version: version,
		timeout: c.cfg.modelsTimeout(),
	}
	status, body, err := c.doJSON(ctx, spec)
	if err != nil {
		return nil, c.classifyErrFor(acct, err)
	}
	if status != http.StatusOK {
		return nil, c.classifyHTTP("models", acct.ID, status, body)
	}
	raw, err := decodeEnvelope("models", body)
	if err != nil {
		return nil, c.classifyErrFor(acct, err)
	}
	models, err := parseModelList(raw)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("models: the upstream returned an empty model list")
	}
	c.noteSuccess(acct)
	c.storeModels(models)
	return models, nil
}

// --- cache ------------------------------------------------------------------

// cachedModels returns the cached catalogue when it is still fresh.
func (c *Client) cachedModels() ([]core.Model, bool) {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	if len(c.models) == 0 || c.modelsAt.IsZero() {
		return nil, false
	}
	if c.now().Sub(c.modelsAt) > c.cfg.modelsTTL() {
		return nil, false
	}
	return copyModels(c.models), true
}

// lastModels returns the cached catalogue whatever its age.  It is what a
// failed refresh falls back to, so an outage never empties the model picker.
func (c *Client) lastModels() []core.Model {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	return copyModels(c.models)
}

func (c *Client) storeModels(models []core.Model) {
	c.modelsMu.Lock()
	c.models = copyModels(models)
	c.modelsAt = c.now()
	c.modelsMu.Unlock()
}

// statusModels is the catalogue Status reports: whatever was fetched, else the
// built-in list.  It never touches the network.
func (c *Client) statusModels() []core.Model {
	if models := c.lastModels(); len(models) > 0 {
		return models
	}
	return fallbackModels(c.cfg)
}

// modelIDs renders the catalogue as bare ids.
func (c *Client) modelIDs() []string {
	models := c.statusModels()
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

// NOTE: this module deliberately does NOT implement core.ModelLimitsProvider.
// The interface is for models whose maximum output is known, and the only way to
// know it would be to fetch the model list -- which is forbidden on that path
// because the gateway calls it on every request that omitted max_tokens.  Since
// /api/models/available publishes no output budget at all, the honest answer is
// to not implement the interface rather than to invent a number or to fetch
// inside a chat request.
