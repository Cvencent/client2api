package core

import (
	"sort"
	"strings"
)

// modelDisplayID strips the vendor namespace from a model id while preserving
// the vendor's spelling.  Cline's `cline-free/deepseek-v4.1-flash`,
// OpenRouter's `deepseek/deepseek-v4.1-flash` and WorkBuddy's
// `cn:deepseek-v4.1-flash` all become `deepseek-v4.1-flash`; a bare id is
// returned unchanged.
func modelDisplayID(model string) string {
	name := strings.TrimSpace(model)
	s := strings.ToLower(name)
	if i := strings.IndexByte(s, ':'); i > 0 {
		switch {
		case s[:i] == "cn", s[:i] == "global":
			name = strings.TrimSpace(name[i+1:])
		case s[i+1:] == "free":
			name = strings.TrimSpace(name[:i])
		}
	}
	if i := strings.LastIndexByte(strings.ToLower(name), '/'); i >= 0 {
		name = strings.TrimSpace(name[i+1:])
	}
	return name
}

// ModelDisplayID is modelDisplayID for the gateway's Auto/ catalogue.  It is
// exported so the catalogue groups vendor-namespaced ids exactly the way the
// router does instead of growing a second, drifting copy of the rule.
func ModelDisplayID(model string) string { return modelDisplayID(model) }

// CanonicalModelID is the exported routing key used by the Auto/ catalogue.
// It is deliberately the lowercase form: one vendor's `DeepSeek-V4.1-Flash`
// and another's `deepseek-v4.1-flash` must collapse to one Auto entry.
func CanonicalModelID(model string) string { return canonicalModelID(model) }

// canonicalModelID is the routing key used when an operator asks for a bare
// model id and every vendor publishes its own namespace around it.  The
// vendor prefix is not part of the model identity for routing: Cline's
// `cline-free/deepseek-v4.1-flash`, OpenRouter's
// `deepseek/deepseek-v4.1-flash` and WorkBuddy's `cn:deepseek-v4.1-flash`
// all name the same underlying model.
func canonicalModelID(model string) string {
	return normalizeModelID(modelDisplayID(model))
}

// selectCatalogModels returns every catalogue entry that can satisfy a
// requested id on one platform.  Exact ids still collapse to themselves; the
// only expansion is the namespace-equivalent fallback, where several realm
// ids can name the same bare model.  Returning all of them lets the caller
// try the sibling realm when the first one has no usable account.
func selectCatalogModels(models []Model, requested string, allowEquivalent bool, allowed func(Model) bool) ([]Model, bool, bool) {
	exactFound := false
	for _, m := range models {
		if !strings.EqualFold(strings.TrimSpace(m.ID), requested) {
			continue
		}
		exactFound = true
		if allowed(m) {
			return []Model{m}, true, false
		}
	}
	if exactFound || !allowEquivalent {
		return nil, false, exactFound
	}

	key := canonicalModelID(requested)
	var (
		matches []Model
		blocked bool
	)
	for _, m := range models {
		if canonicalModelID(m.ID) != key {
			continue
		}
		if !allowed(m) {
			blocked = true
			continue
		}
		matches = append(matches, m)
	}
	if len(matches) > 0 {
		// Preserve the historical "free sibling first" preference while
		// keeping the remaining equivalents available for failover.
		sort.SliceStable(matches, func(i, j int) bool {
			return modelFree(matches[i]) && !modelFree(matches[j])
		})
		return matches, true, false
	}
	return nil, false, blocked
}

// selectCatalogModel picks the entry a platform should receive for a request.
// Exact ids always win.  A bare request may also fall through to namespace-
// equivalent ids, with a free equivalent preferred when a platform publishes
// both a free and a metered sibling.  The bools report whether a match was
// allowed and whether a match existed but the platform blacklisted it.
func selectCatalogModel(models []Model, requested string, allowEquivalent bool, allowed func(Model) bool) (Model, bool, bool) {
	exactFound := false
	for _, m := range models {
		if !strings.EqualFold(strings.TrimSpace(m.ID), requested) {
			continue
		}
		exactFound = true
		if allowed(m) {
			return m, true, false
		}
	}
	if exactFound || !allowEquivalent {
		return Model{}, false, exactFound
	}

	key := canonicalModelID(requested)
	var best Model
	found, allowedFound, blocked := false, false, false
	for _, m := range models {
		if canonicalModelID(m.ID) != key {
			continue
		}
		found = true
		if !allowed(m) {
			blocked = true
			continue
		}
		if !allowedFound || (modelFree(m) && !modelFree(best)) {
			best = m
			allowedFound = true
		}
	}
	if allowedFound {
		return best, true, false
	}
	return Model{}, false, found && blocked
}
