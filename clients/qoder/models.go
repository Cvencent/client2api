package qoder

import (
	"strings"

	"client2api/internal/core"
)

// models.go owns the catalogue.
//
// The vendor's own model list lives at GET https://gateway.qoder.com.cn/api/v2/model/list,
// behind the same native request signature the inference host requires (see
// README).  This module therefore does NOT pretend to fetch it: the catalogue is
// the operator's `models` list when one is configured, and a short built-in list
// of the ids the desktop client's own logs show it asking for otherwise.

// builtinModelIDs is the fallback catalogue.  These are the model keys Qoder CN
// 1.32 asks its gateway for; they are not guesses about new models, they are the
// spellings observed in the client's own request log.
func builtinModelIDs() []string {
	return []string{"qmodel_38max", "qfmodel"}
}

// catalogue renders model ids as the core catalogue.
func catalogue(ids []string) []core.Model {
	out := make([]core.Model, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		out = append(out, core.Model{
			ID:      id,
			OwnedBy: "qoder",
			Extra:   map[string]any{"platform": "qoder"},
		})
	}
	return out
}
