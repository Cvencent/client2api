package panel

import (
	"encoding/json"
	"net/http"
	"os"
	"time"

	"client2api/internal/core"
)

// DefaultProbeFileName is the contract file probe_max_tokens.py writes with
// --panel-out.  It is looked up next to the usage file, i.e. derived from the
// configured data directory, so an operator who moves the data directory moves
// the probe results with it and never has to add a second path setting.
const DefaultProbeFileName = "output_probes.json"

// handleModelProbes serves GET /panel/api/model_probes: a read-only passthrough
// of the model output-limit probe results, rendered by the models view as a
// "measured" annotation next to the value the vendor claims.
//
// Design boundaries, kept deliberately identical to the reference:
//   - The gateway only relays the document.  It never parses the per-model
//     fields (claimed/measured/verdict/note/tested_at/source), never routes on
//     them and never reads them on the request path.
//   - Unconfigured file, or a file that does not exist, is an empty set with
//     HTTP 200, not an error: "no data" legitimately means "no annotation", and
//     a fresh install must not show a red banner over an optional extra.
//   - A file that exists but cannot be parsed is a 502, so a broken probe run
//     is visible instead of silently degrading to the un-annotated table.
//   - Re-running the probe tool takes effect on the next query; no gateway
//     restart and no cache.
//
// The one addition over the reference is that error text goes through
// core.Redact.  That is defence in depth, not path hiding.  What the two errors
// say matches the reference exactly: a read failure keeps the path because the
// OS error carries it (the reader is the authenticated operator who has to know
// which file to fix), while a parse failure reports only the parser's complaint.
func (p *panel) handleModelProbes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	out := map[string]any{"probes": map[string]json.RawMessage{}, "exists": false}
	if p.opts.ProbeFile == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	raw, err := os.ReadFile(p.opts.ProbeFile)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, out)
			return
		}
		writeErr(w, http.StatusInternalServerError, "read probes: "+core.Redact(err.Error()))
		return
	}
	var f struct {
		Version int                        `json:"version"`
		Probes  map[string]json.RawMessage `json:"probes"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		writeErr(w, http.StatusBadGateway, "parse probes: "+core.Redact(err.Error()))
		return
	}
	if f.Probes == nil {
		f.Probes = map[string]json.RawMessage{}
	}
	out["probes"] = f.Probes
	out["exists"] = true
	// Best effort: the modtime is what lets the frontend say "measured 40 days
	// ago".  A stat that fails after a successful read is not worth an error.
	if fi, err := os.Stat(p.opts.ProbeFile); err == nil {
		out["updated_at"] = fi.ModTime().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
}
