package core

import "strings"

// canonicalModelID is the routing key used when an operator asks for a bare
// model id and every vendor publishes its own namespace around it.  The
// vendor prefix is not part of the model identity for routing: Cline's
// `cline-free/deepseek-v4.1-flash`, OpenRouter's
// `deepseek/deepseek-v4.1-flash` and WorkBuddy's `cn:deepseek-v4.1-flash`
// all name the same underlying model.
func canonicalModelID(model string) string {
	s := normalizeModelID(model)
	if i := strings.IndexByte(s, ':'); i > 0 {
		switch {
		case s[:i] == "cn", s[:i] == "global":
			s = s[i+1:]
		case s[i+1:] == "free":
			s = s[:i]
		}
	}
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
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
