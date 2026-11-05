package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"client2api/internal/core"
)

var sourceEditMu sync.Mutex

// platformPolicyPresent reports whether the platforms table holds anything at
// all for id.  An entry that exists but is empty counts: the panel draws a card
// for it, so the operator has something to remove.  A null entry does not --
// it is already the "no policy" state.
func platformPolicyPresent(cfg map[string]any, id string) bool {
	platforms, _ := cfg["platforms"].(map[string]any)
	entry, ok := platforms[id]
	if !ok || entry == nil {
		return false
	}
	if m, ok := entry.(map[string]any); ok {
		return len(m) > 0
	}
	return true
}

func (p *panel) handleSource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		writeErr(w, 405, "use PUT or DELETE")
		return
	}
	if p.opts.Reload == nil || p.opts.Registry == nil {
		writeErr(w, http.StatusServiceUnavailable, "source editing requires a live registry and hot reload")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/panel/api/sources/")
	if err := core.ValidateSources(map[string]core.SourceConfig{id: {BaseURL: "http://127.0.0.1/v1"}}); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if r.Method == http.MethodDelete {
		if _, builtin := core.BuiltinSource(id); builtin {
			writeErr(w, http.StatusBadRequest, "built-in platforms cannot be deleted; edit the explicit sources entry in the config file to disable or override it")
			return
		}
	}
	sourceEditMu.Lock()
	defer sourceEditMu.Unlock()
	var entry any
	if r.Method == http.MethodPut {
		var fields map[string]any
		if !decodeJSON(w, r, &fields) {
			return
		}
		raw, err := json.Marshal(map[string]any{id: fields})
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if _, err := core.ParseSources(raw); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		// Keep the validated request object intact. Rebuilding from SourceConfig
		// would drop explicit zeros (notably max_in_flight: 0) and any future
		// optional field before it reaches the config file.
		entry = fields
	} else {
		cfg, err := p.readConfigMap()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		sources, _ := cfg["sources"].(map[string]any)
		if _, ok := sources[id]; !ok {
			// No source definition, but the platform may still own routing
			// policy on disk -- a model_priorities table, a blacklist, an
			// account note.  That residue is what keeps the card on the
			// platform page after "删除", so a delete that only ever removed
			// the source entry left the operator with something they could
			// see but not get rid of.  Clearing the policy too is what the
			// button promises; a request that names neither is still a 404.
			if !platformPolicyPresent(cfg, id) {
				writeErr(w, 404, "source not found")
				return
			}
			entry = nil
		}
	}
	patch := map[string]any{"sources": map[string]any{id: entry}}
	if r.Method == http.MethodDelete {
		patch["platforms"] = map[string]any{id: nil}
	}
	if _, _, err := writeConfigKeys(p.opts.ConfigPath, patch, p.opts.Registry); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := p.opts.Reload(); err != nil {
		writeErr(w, 500, fmt.Sprintf("source saved but reload failed: %s", core.Redact(err.Error())))
		return
	}
	writeJSON(w, 200, map[string]any{"saved": true, "id": id, "restart_required": false})
}
