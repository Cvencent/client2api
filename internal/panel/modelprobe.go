package panel

import (
	"encoding/json"
	"net/http"
)

// probeOutcomeKnown marks a model that is settled because its context window is
// already known, without a probe round trip having been made.
const probeOutcomeKnown = "known"

// modelProbeView is the panel-facing shape of one model's probe verdict.
type modelProbeView struct {
	Probed         bool   `json:"probed"`
	Outcome        string `json:"outcome"`
	EstimateTokens int64  `json:"estimate_tokens,omitempty"`
	VendorTokens   int64  `json:"vendor_tokens,omitempty"`
	At             string `json:"at,omitempty"`
}

// probeViewFor returns the row's probe badge, or nil when the model is not
// settled.
//
// A model counts as settled when either a probe recorded a verdict or the
// gateway already knows its context window -- from the vendor catalogue, a
// learned refusal, or the operator's own entry.  Knowing the number is what the
// badge is about, so the table shows "already probed" by default and only an
// explicit clear makes a model look unprobed again.
func probeViewFor(probes ProbeSource, client, model string, contextLength int64) *modelProbeView {
	if probes == nil {
		if contextLength > 0 {
			return &modelProbeView{Probed: true, Outcome: probeOutcomeKnown}
		}
		return nil
	}
	rec, ok := probes.Snapshot()[client][model]
	if ok {
		if !rec.Probed {
			// An explicit clear: show the neutral state so the operator can
			// see that the next over-budget request will probe again.
			return nil
		}
		view := &modelProbeView{
			Probed:         true,
			Outcome:        rec.Outcome,
			EstimateTokens: rec.EstimateTokens,
			VendorTokens:   rec.VendorTokens,
		}
		if !rec.At.IsZero() {
			view.At = rec.At.Format("2006-01-02T15:04:05Z07:00")
		}
		return view
	}
	if contextLength > 0 {
		return &modelProbeView{Probed: true, Outcome: probeOutcomeKnown}
	}
	return nil
}

// handleModelProbeClear forgets one (platform, model) probe so the next
// oversized request probes the vendor again.
//
// It touches only the probe record: the learned window and any operator
// override are separate stores and survive, because clearing a measurement is
// not a request to forget what the vendor said or what a person decided.
func (p *panel) handleModelProbeClear(w http.ResponseWriter, r *http.Request) {
	if p.opts.Probes == nil {
		writeErr(w, http.StatusNotImplemented, "context probes are not wired")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body struct {
		Client string `json:"client"`
		Model  string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Client == "" || body.Model == "" {
		writeErr(w, http.StatusBadRequest, "client and model are required")
		return
	}
	p.opts.Probes.Clear(body.Client, body.Model)
	writeJSON(w, http.StatusOK, map[string]any{
		"cleared": true,
		"client":  body.Client,
		"model":   body.Model,
	})
}
