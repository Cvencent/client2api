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
	sourceEditMu.Lock()
	defer sourceEditMu.Unlock()
	var entry any
	if r.Method == http.MethodPut {
		var cfg core.SourceConfig
		var fields map[string]any
		if !decodeJSON(w, r, &fields) {
			return
		}
		raw, err := json.Marshal(map[string]any{id: fields})
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		sources, err := core.ParseSources(raw)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		cfg = sources[id]
		entry = map[string]any{"label": cfg.Label, "base_url": cfg.BaseURL,
			"disabled": cfg.Disabled, "max_tokens_field": cfg.MaxTokensField}
	} else {
		cfg, err := p.readConfigMap()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		sources, _ := cfg["sources"].(map[string]any)
		if _, ok := sources[id]; !ok {
			writeErr(w, 404, "source not found")
			return
		}
	}
	patch := map[string]any{"sources": map[string]any{id: entry}}
	if r.Method == http.MethodDelete {
		patch["platforms"] = map[string]any{id: nil}
	}
	if _, _, err := writeConfigKeys(p.opts.ConfigPath, patch); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := p.opts.Reload(); err != nil {
		writeErr(w, 500, fmt.Sprintf("source saved but reload failed: %s", core.Redact(err.Error())))
		return
	}
	writeJSON(w, 200, map[string]any{"saved": true, "id": id, "restart_required": false})
}
