package panel

import (
	"net/http"

	"client2api/internal/alerts"
	"client2api/internal/core"
)

// handleAlerts serves the platform-health notification journal, newest first.
func (p *panel) handleAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	out := struct {
		Alerts    []alerts.Alert `json:"alerts"`
		LastError string         `json:"last_error,omitempty"`
	}{Alerts: []alerts.Alert{}}
	if p.opts.Alerts != nil {
		out.Alerts = p.opts.Alerts.List()
		if out.Alerts == nil {
			out.Alerts = []alerts.Alert{}
		}
		if err := p.opts.Alerts.LastError(); err != nil {
			out.LastError = core.Redact(err.Error())
		}
	}
	writeJSON(w, http.StatusOK, out)
}
