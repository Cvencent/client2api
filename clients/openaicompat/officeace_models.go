package openaicompat

import (
	"context"
	"strings"
)

var officeAceDesktopBaseModels = map[string]bool{
	"deepseek-v4-flash-0731": true,
	"deepseek-v4.1-flash":    true,
	"glm-5":                  true,
	"glm-5.1":                true,
	"glm-5.2":                true,
	"glm-5.3-flash":          true,
}

// enableDesktopModelSelection keeps OfficeAce's raw /models catalogue from
// leaking ids that its own desktop picker filters out.
func (o *officeAceSource) enableDesktopModelSelection() {
	if o == nil || o.source == nil {
		return
	}
	o.source.scanFilter = o.filterDesktopModels
	o.source.wireModel = officeAceWireModel
}

func (o *officeAceSource) filterDesktopModels(ctx context.Context, p storedProvider, list []modelCatalogueEntry) ([]modelCatalogueEntry, error) {
	ultimate := false
	if p.OfficeAce != nil && p.OfficeAce.usable() {
		premium, err := o.premiumFor(p.ID)
		if err == nil {
			if sub, serr := premium.subscription(ctx); serr == nil {
				ultimate = officeAceUltimateSubscription(sub)
			}
		}
	}

	allowed := make(map[string]bool, len(officeAceDesktopBaseModels)+1)
	for id := range officeAceDesktopBaseModels {
		allowed[id] = true
	}
	if ultimate {
		allowed["glm-5.3"] = true
	}

	out := make([]modelCatalogueEntry, 0, len(list))
	seen := map[string]bool{}
	for _, model := range list {
		id := officeAceDesktopModelID(model.ID)
		key := strings.ToLower(strings.TrimSpace(id))
		if !allowed[key] || seen[key] {
			continue
		}
		seen[key] = true
		model.ID = id
		out = append(out, model)
	}
	return out, nil
}

func officeAceUltimateSubscription(sub officeAceSubscription) bool {
	for _, sku := range sub.Skus {
		if strings.Contains(strings.ToLower(strings.TrimSpace(sku.Code)), ".ultimate") {
			return true
		}
	}
	return false
}

// Huawei advertises glm-5.3-flash in /v2/models but its chat endpoint answers
// 404 Invalid model for that id. The OfficeAce chat route accepts the same
// request under glm-5.3, so keep the public id stable and translate only at
// the wire boundary.
func officeAceWireModel(raw string) string {
	id := strings.TrimSpace(raw)
	if strings.EqualFold(id, "glm-5.3-flash") {
		return "glm-5.3"
	}
	return id
}

// The desktop client strips a trailing -zx only for its five standard ids.
func officeAceDesktopModelID(raw string) string {
	id := strings.TrimSpace(raw)
	if !strings.HasSuffix(strings.ToLower(id), "-zx") {
		return id
	}
	base := strings.TrimSpace(id[:len(id)-3])
	if officeAceDesktopBaseModels[strings.ToLower(base)] {
		return base
	}
	return id
}
