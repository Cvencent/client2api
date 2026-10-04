package panel

import (
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
	writeJSON(w, http.StatusOK, out)
}
