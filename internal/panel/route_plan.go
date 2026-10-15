package panel

import (
	"net/http"
	"strings"

	"client2api/internal/core"
)

// routePlanRequest is the body of POST /panel/api/route_plan.  catalogs is the
// caller"s own snapshot of each platform"s model list (the panel page already
// holds it from /panel/api/models), so this endpoint never asks a module for
// anything: no cache refresh, no account reads, no upstream traffic at all.
type routePlanRequest struct {
	Model    string                            `json:"model"`
	Catalogs map[string][]routePlanCatalogItem `json:"catalogs"`
}

type routePlanCatalogItem struct {
	ID    string         `json:"id"`
	Free  bool           `json:"free"`
	Extra map[string]any `json:"extra,omitempty"`
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
	req.Model = strings.TrimSpace(req.Model)
	if req.Model == "" {
		writeErr(w, http.StatusBadRequest, "model is required")
		return
	}
	if p.opts.Registry == nil {
		writeErr(w, http.StatusServiceUnavailable, "no registry")
		return
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
			models = append(models, m)
		}
		catalogs[name] = models
	}

	plan, err := p.opts.Registry.ResolveStaticCatalog(req.Model, catalogs)
	if err != nil {
		status := http.StatusNotFound
		if strings.Contains(err.Error(), "model is required") {
			status = http.StatusBadRequest
		}
		writeErr(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
}
