package panel

import (
	"net/http"
	"strconv"

	"client2api/internal/gateway"
)

// defaultLogLines is what /panel/api/logs returns when the caller does not say.
const defaultLogLines = 500

// logsResponse carries the retained log window.  Entries rather than bare
// strings, because the view filters by channel: the gateway's log stream is one
// interleaved feed, and chat traffic alone can fill the ring before an operator
// ever looks at a task result.
type logsResponse struct {
	Entries  []gateway.LogEntry `json:"entries"`
	Total    int64              `json:"total"`
	Capacity int                `json:"capacity"`
}

// handleLogs serves the gateway's recent log lines, newest last.  The ring
// buffer already redacts, single-lines and channel-tags every entry as it is
// stored, so this handler only has to clamp the count.
func (p *panel) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	ring := p.opts.Logs
	if ring == nil {
		writeJSON(w, http.StatusOK, logsResponse{Entries: []gateway.LogEntry{}})
		return
	}

	limit := defaultLogLines
	if raw := r.URL.Query().Get("lines"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "lines must be an integer")
			return
		}
		if n > 0 {
			limit = n
		}
	}
	// Hard max is the ring's capacity: asking for more than is retained is
	// harmless but pointless, and clamping keeps the response bounded.
	if limit > ring.Capacity() {
		limit = ring.Capacity()
	}

	writeJSON(w, http.StatusOK, logsResponse{
		Entries:  ring.Entries(limit),
		Total:    ring.Total(),
		Capacity: ring.Capacity(),
	})
}
