package panel

import (
	"context"
	"net/http"
	"sync"
	"time"

	"client2api/internal/core"
)

// Per-client budget for a catalogue call.  The overall request budget below is
// slightly larger so that the last client to finish still has room to answer
// rather than being cut off by the outer deadline.
const (
	modelsClientTimeout = 5 * time.Second
	modelsTotalTimeout  = 8 * time.Second
)

type modelInfo struct {
	ID      string         `json:"id"`
	OwnedBy string         `json:"owned_by"`
	Extra   map[string]any `json:"extra"`
}

type clientModels struct {
	Name       string      `json:"name"`
	CanRefresh bool        `json:"can_refresh"`
	Error      string      `json:"error"`
	Models     []modelInfo `json:"models"`
}

type modelsReport struct {
	RefreshedAt string         `json:"refreshed_at"`
	Clients     []clientModels `json:"clients"`
}

// handleModelsList reports each module's catalogue as the module itself
// presents it.  Unlike /v1/models this is NOT flattened: the panel groups by
// client, so the client boundary is the whole point.
func (p *panel) handleModelsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	p.serveModels(w, r, false)
}

// handleModelsRefresh re-reads every catalogue, calling RefreshModels on the
// modules that actually implement it.  A module that does not implement it is
// served from its plain Models: claiming a refresh happened would be a lie.
func (p *panel) handleModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	p.serveModels(w, r, true)
}

func (p *panel) serveModels(w http.ResponseWriter, r *http.Request, refresh bool) {
	ctx, cancel := p.ctx(r, modelsTotalTimeout)
	defer cancel()

	var clients []core.Client
	if p.opts.Registry != nil {
		clients = p.opts.Registry.All()
	}

	// Every client gets its own goroutine and its own slice slot, so a slow
	// module delays only itself and the results need no lock.
	out := make([]clientModels, len(clients))
	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Add(1)
		p.safeGo("panel models", func() {
			defer wg.Done()
			cctx, ccancel := context.WithTimeout(ctx, modelsClientTimeout)
			defer ccancel()
			out[i] = p.modelsFor(cctx, c, refresh)
		})
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, modelsReport{
		RefreshedAt: time.Now().UTC().Format(time.RFC3339),
		Clients:     out,
	})
}

func (p *panel) modelsFor(ctx context.Context, c core.Client, refresh bool) clientModels {
	row := clientModels{Name: c.Name(), Models: []modelInfo{}}

	refresher, canRefresh := core.AsModelRefresher(c)
	row.CanRefresh = canRefresh

	var (
		list []core.Model
		err  error
	)
	if refresh && canRefresh {
		// RefreshModels is documented to return its last good list alongside
		// the error, so the error must never blank the catalogue.
		list, err = refresher.RefreshModels(ctx)
	} else {
		list, err = c.Models(ctx)
	}

	if err != nil {
		row.Error = core.Redact(err.Error())
		if len(list) == 0 {
			// Showing nothing with no explanation is the worst outcome; say
			// that the list is empty and why.
			row.Error += " (no models returned)"
		}
	}

	for _, m := range list {
		extra, _ := core.RedactAny(m.Extra).(map[string]any)
		if extra == nil {
			extra = map[string]any{}
		}
		owned := m.OwnedBy
		if owned == "" {
			owned = c.Name()
		}
		row.Models = append(row.Models, modelInfo{
			ID:      core.Redact(m.ID),
			OwnedBy: core.Redact(owned),
			Extra:   extra,
		})
	}
	return row
}
