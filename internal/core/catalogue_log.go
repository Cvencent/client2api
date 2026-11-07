package core

import (
	"sort"
	"strings"
)

// LogNewCatalogueIDs announces models the vendor's catalogue gained since the
// last cache (issue #237): the module stores them silently, and a model that
// never reaches exposed_models never reaches a caller either -- without this
// line the operator had no way to know the vendor had added anything.
func LogNewCatalogueIDs(logf func(string, ...any), module string, previous, current []Model) {
	if logf == nil {
		return
	}
	known := make(map[string]struct{}, len(previous))
	for _, m := range previous {
		known[strings.ToLower(m.ID)] = struct{}{}
	}
	var fresh []string
	for _, m := range current {
		if _, dup := known[strings.ToLower(m.ID)]; dup {
			continue
		}
		fresh = append(fresh, m.ID)
	}
	if len(fresh) == 0 {
		return
	}
	sort.Strings(fresh)
	logf("%s: vendor catalogue has %d new model(s): %s（不在 exposed_models 时调用方不可见；如需曝光请在配置中添加）",
		module, len(fresh), strings.Join(fresh, ", "))
}
