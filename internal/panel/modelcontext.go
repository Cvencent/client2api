package panel

import (
	"net/http"

	"client2api/internal/modelmeta"
)

type modelContextBody struct {
	Clients map[string]map[string]modelmeta.Override `json:"clients"`
}

// handleModelContext reads or replaces the operator's manual model metadata.
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
			for model, override := range models {
				if override.IsZero() {
					if err := p.opts.ModelOverrides.Delete(client, model); err != nil {
						writeErr(w, http.StatusBadRequest, err.Error())
						return
					}
					continue
				}
				if err := p.opts.ModelOverrides.Set(client, model, override.ContextLength, override.MaxOutputTokens); err != nil {
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
