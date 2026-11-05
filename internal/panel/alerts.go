package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"client2api/internal/alerts"
	"client2api/internal/core"
)

// handleAlerts serves the platform-health notification journal, newest first.
// POST marks the journal read; the badge is the unread count, not the history
// length, so opening the page has to clear it.
func (p *panel) handleAlerts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		if p.opts.Alerts != nil {
			p.opts.Alerts.MarkAllRead()
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	case http.MethodDelete:
		if p.opts.Alerts != nil {
			p.opts.Alerts.Clear()
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cleared": true})
		return
	case http.MethodGet:
	default:
		writeErr(w, http.StatusMethodNotAllowed, "use GET or POST")
		return
	}
	// count=1 是徽标专用的轻查询（issue #208）：5 秒一拍的
	// refreshAlertBadge 只读 unread 一个整数，却曾把整本流水（上限 200 条
	// ≈ 50KB，一拍的八成）每拍搬回来。照 logs.go 的 meta=1 范式。
	if r.URL.Query().Get("count") == "1" {
		out := struct {
			Unread int `json:"unread"`
		}{}
		if p.opts.Alerts != nil {
			out.Unread = p.opts.Alerts.Unread()
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	out := struct {
		Alerts    []alerts.Alert `json:"alerts"`
		Unread    int            `json:"unread"`
		LastError string         `json:"last_error,omitempty"`
	}{Alerts: []alerts.Alert{}}
	if p.opts.Alerts != nil {
		out.Alerts = p.opts.Alerts.List()
		if out.Alerts == nil {
			out.Alerts = []alerts.Alert{}
		}
		out.Unread = p.opts.Alerts.Unread()
		if err := p.opts.Alerts.LastError(); err != nil {
			out.LastError = core.Redact(err.Error())
		}
	}
	// 强 ETag + If-None-Match（issue #208）：只对 alerts[] 求哈希，不算
	// unread——标记已读只改 unread，若把它算进去 304 就永远失效。
	body, err := json.Marshal(out.Alerts)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "marshal alerts: "+err.Error())
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	w.Header().Set("ETag", etag)
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
