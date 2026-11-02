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

// logsMetaResponse is the two integers the 4-second poll actually needs
// (issue #92): with them the page can tell "nothing new" from "changed"
// without downloading the 500-line body every tick.  ~100 bytes against
// ~133 KB; a quiet gateway then moves zero bytes per poll instead of
// re-sending identical content.  It is a query parameter rather than a
// separate route so the existing dedupe stamp keeps one authority for
// "what was last seen".
type logsMetaResponse struct {
	Total    int64 `json:"total"`
	Capacity int   `json:"capacity"`
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

	// The lightweight probe: total + capacity only, no entries.  The page's
	// 4-second poll asks this first and fetches the full window only when
	// total moved (issue #92).
	if r.URL.Query().Get("meta") == "1" {
		writeJSON(w, http.StatusOK, logsMetaResponse{
			Total:    ring.Total(),
			Capacity: ring.Capacity(),
		})
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
