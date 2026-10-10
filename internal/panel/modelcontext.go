package panel

import (
	"net/http"

	"client2api/internal/modelmeta"
)

type modelContextBody struct {
	Clients map[string]map[string]modelContextPatch `json:"clients"`
}

// modelContextPatch distinguishes an omitted JSON field from an explicit zero,
// so saving one metadata field never clears another.
type modelContextPatch struct {
	ContextLength       *int64   `json:"context_length"`
	MaxOutputTokens     *int64   `json:"max_output_tokens"`
	InputPerMillion     *float64 `json:"input_per_million"`
	OutputPerMillion    *float64 `json:"output_per_million"`
	CacheReadPerMillion *float64 `json:"cache_read_per_million"`
	HasPrice            *bool    `json:"has_price"`
	HasCacheRead        *bool    `json:"has_cache_read"`
}

func (p modelContextPatch) touchesPrice() bool {
	return p.HasPrice != nil || p.HasCacheRead != nil ||
		p.InputPerMillion != nil || p.OutputPerMillion != nil || p.CacheReadPerMillion != nil
}

// handleModelContext reads or applies the operator's manual model metadata.
func (p *panel) handleModelContext(w http.ResponseWriter, r *http.Request) {
	if p.opts.ModelOverrides == nil {
		writeErr(w, http.StatusNotImplemented, "model metadata overrides are not wired")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"clients": p.opts.ModelOverrides.Snapshot()})
	case http.MethodPost:
		var body modelContextBody
		if !decodeJSON(w, r, &body) {
			return
		}
		for client, models := range body.Clients {
			for model, patch := range models {
				if err := applyModelContextPatch(p.opts.ModelOverrides, client, model, patch); err != nil {
					writeErr(w, http.StatusBadRequest, err.Error())
					return
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"clients": p.opts.ModelOverrides.Snapshot()})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "use GET or POST")
	}
}

func applyModelContextPatch(store *modelmeta.OverrideStore, client, model string, patch modelContextPatch) error {
	current, _ := store.Get(client, model)
	next := current

	if patch.ContextLength != nil {
		next.ContextLength = *patch.ContextLength
	}
	if patch.MaxOutputTokens != nil {
		next.MaxOutputTokens = *patch.MaxOutputTokens
	}

	if patch.touchesPrice() {
		hasPrice := current.HasPrice
		if patch.HasPrice != nil {
			hasPrice = *patch.HasPrice
		} else if patch.InputPerMillion != nil || patch.OutputPerMillion != nil {
			hasPrice = true
		}
		if !hasPrice {
			next.InputPerMillion = 0
			next.OutputPerMillion = 0
			next.CacheReadPerMillion = 0
			next.HasPrice = false
			next.HasCacheRead = false
		} else {
			if patch.InputPerMillion != nil {
				next.InputPerMillion = *patch.InputPerMillion
			}
			if patch.OutputPerMillion != nil {
				next.OutputPerMillion = *patch.OutputPerMillion
			}
			if patch.CacheReadPerMillion != nil {
				next.CacheReadPerMillion = *patch.CacheReadPerMillion
			}
			if patch.HasCacheRead != nil {
				next.HasCacheRead = *patch.HasCacheRead
				if !next.HasCacheRead {
					next.CacheReadPerMillion = 0
				}
			}
			next.HasPrice = true
		}
	}

	if err := store.Set(client, model, next.ContextLength, next.MaxOutputTokens); err != nil {
		return err
	}
	if !patch.touchesPrice() {
		return nil
	}
	if next.HasPrice {
		return store.SetPrice(client, model, next.InputPerMillion, next.OutputPerMillion, next.CacheReadPerMillion, next.HasCacheRead)
	}
	return store.DeletePrice(client, model)
}
