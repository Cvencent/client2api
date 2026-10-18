package panel

import (
	"errors"
	"net/http"
	"strings"

	"client2api/internal/core"
)

// routePlanRequest is the shared body shape for the route-plan and route-speed
// endpoints.  catalogs is the caller's own snapshot of each platform's model
// list, taken from /panel/api/models.  Route-plan is purely local; route-speed
// uses the same resolved plan and then probes each platform live.
type routePlanRequest struct {
	Model    string                            `json:"model"`
	Catalogs map[string][]routePlanCatalogItem `json:"catalogs"`
}

type routePlanCatalogItem struct {
	ID            string         `json:"id"`
	Free          bool           `json:"free"`
	ContextLength int64          `json:"context_length,omitempty"`
	ContextSource string         `json:"context_source,omitempty"`
	Extra         map[string]any `json:"extra,omitempty"`
}

// handleRoutePlan resolves the static priority chain for one model.  The
// response carries every platform that would be tried, in failover order,
// with the effective priority and which rule set it.
func (p *panel) handleRoutePlan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req routePlanRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	plan, err := p.resolveRoutePlan(req)
	if err != nil {
		status := http.StatusNotFound
		if strings.Contains(err.Error(), "model is required") {
			status = http.StatusBadRequest
		} else if strings.Contains(err.Error(), "no registry") {
			status = http.StatusServiceUnavailable
		}
		writeErr(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// resolveRoutePlan turns the panel's already-cached catalogue snapshot into
// the same static chain the route-plan and route-speed handlers expose.
func (p *panel) resolveRoutePlan(req routePlanRequest) (core.RoutePlan, error) {
	req.Model = strings.TrimSpace(req.Model)
	if req.Model == "" {
		return core.RoutePlan{}, errors.New("model is required")
	}
	if p.opts.Registry == nil {
		return core.RoutePlan{}, errors.New("no registry")
	}

	catalogs := make(map[string][]core.Model, len(req.Catalogs))
	for name, items := range req.Catalogs {
		models := make([]core.Model, 0, len(items))
		for _, item := range items {
			id := strings.TrimSpace(item.ID)
			if id == "" {
				continue
			}
			m := core.Model{ID: id}
			if item.Free {
				if m.Extra == nil {
					m.Extra = map[string]any{}
				}
				m.Extra["free"] = true
			}
			if len(item.Extra) > 0 {
				if m.Extra == nil {
					m.Extra = map[string]any{}
				}
				for k, v := range item.Extra {
					m.Extra[k] = v
				}
			}
			if item.ContextLength > 0 {
				if m.Extra == nil {
					m.Extra = map[string]any{}
				}
				m.Extra["context_length"] = item.ContextLength
			}
			if source := strings.TrimSpace(item.ContextSource); source != "" {
				if m.Extra == nil {
					m.Extra = map[string]any{}
				}
				m.Extra["context_source"] = source
			}
			models = append(models, m)
		}
		catalogs[name] = models
	}
	return p.opts.Registry.ResolveStaticCatalog(req.Model, catalogs)
}
