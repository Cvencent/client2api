package openaicompat

import (
	"context"
	"strings"
)

var officeAceDesktopModels = map[string]bool{
	"deepseek-v4-flash-0731": true,
	"deepseek-v4.1-flash":    true,
	"glm-5.1":                true,
	"glm-5.2":                true,
}

// enableDesktopModelSelection keeps OfficeAce's raw /models catalogue from
// leaking ids that its own desktop picker filters out.
func (o *officeAceSource) enableDesktopModelSelection() {
	if o == nil || o.source == nil {
		return
	}
	o.source.scanFilter = o.filterDesktopModels
}

func (o *officeAceSource) filterDesktopModels(_ context.Context, _ storedProvider, list []modelCatalogueEntry) ([]modelCatalogueEntry, error) {
	out := make([]modelCatalogueEntry, 0, len(list))
	seen := map[string]bool{}
	for _, model := range list {
		id := strings.TrimSpace(model.ID)
		key := strings.ToLower(strings.TrimSpace(id))
		if !officeAceDesktopModels[key] || seen[key] {
			continue
		}
		seen[key] = true
		model.ID = id
		out = append(out, model)
	}
	return out, nil
}
