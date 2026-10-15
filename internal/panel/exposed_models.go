package panel

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"client2api/internal/core"
)

// exposedModel is one row in the final outward-facing catalogue.  It is
// deliberately identity-only: the selector needs the exact id the caller may
// send, not the metadata the model-capability table already renders elsewhere.
type exposedModel struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Platform string   `json:"platform,omitempty"`
	Target   string   `json:"target,omitempty"`
	Members  []string `json:"members,omitempty"`
}

type exposedModelsReport struct {
	Configured bool           `json:"configured"`
	Selected   []string       `json:"selected"`
	Models     []exposedModel `json:"models"`
}

// handleExposedModels returns the complete final catalogue before the exposed
// whitelist is applied.  The UI needs the unselected rows too, otherwise an
// operator could never add a model back after removing it.
func (p *panel) handleExposedModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	ctx, cancel := p.ctx(r, modelsTotalTimeout)
	defer cancel()

	report := exposedModelsReport{
		Selected: []string{},
		Models:   []exposedModel{},
	}
	if p.opts.Registry == nil {
		writeJSON(w, http.StatusOK, report)
		return
	}
	report.Configured = p.opts.Registry.ExposedModelsConfigured()
	report.Selected = p.opts.Registry.ExposedModelIDs()
	if report.Selected == nil {
		report.Selected = []string{}
	}
	report.Models = p.finalExposedModels(ctx)
	writeJSON(w, http.StatusOK, report)
}

// finalExposedModels mirrors the identity-building half of gateway's
// /v1/models handler.  It intentionally excludes the exposed whitelist so the
// settings page can show and edit every final id.
func (p *panel) finalExposedModels(ctx context.Context) []exposedModel {
	if p.opts.Registry == nil {
		return []exposedModel{}
	}
	reg := p.opts.Registry
	seen := make(map[string]struct{}, 128)
	auto := make(map[string]exposedAutoPick)
	rows := make([]exposedModel, 0, 128)

	for _, c := range reg.All() {
		models, err := c.Models(ctx)
		if err != nil {
			continue
		}
		for _, m := range models {
			id := c.Name() + "/" + m.ID
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			if !reg.ModelAllowed(c.Name(), m.ID) {
				continue
			}
			if key := core.CanonicalModelID(m.ID); key != "" {
				pick := exposedAutoPick{display: core.ModelDisplayID(m.ID), raw: m.ID}
				if pick.display == "" {
					pick.display = m.ID
				}
				if old, ok := auto[key]; !ok || betterExposedAutoDisplay(pick, old) {
					auto[key] = pick
				}
			}
			seen[id] = struct{}{}
			rows = append(rows, exposedModel{ID: id, Kind: "platform", Platform: c.Name()})
		}
	}

	for _, c := range reg.All() {
		provider, ok := c.(core.TranscriptionProvider)
		if !ok {
			continue
		}
		for _, m := range provider.TranscriptionModels(ctx) {
			id := c.Name() + "/" + m.ID
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			rows = append(rows, exposedModel{ID: id, Kind: "transcription", Platform: c.Name()})
		}
	}

	for key, pick := range auto {
		id := "Auto/" + pick.display
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		rows = append(rows, exposedModel{ID: id, Kind: "auto", Target: key})
	}

	for name, group := range reg.ModelGroups() {
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		members := make([]string, 0, len(group.Members))
		for _, member := range group.Members {
			members = append(members, member.Client+"/"+member.Model)
		}
		seen[name] = struct{}{}
		rows = append(rows, exposedModel{ID: name, Kind: "group", Members: members})
	}

	for alias, target := range reg.Aliases() {
		if _, duplicate := seen[alias]; duplicate {
			continue
		}
		if platform, model, ok := strings.Cut(target, "/"); ok && platform != "" && model != "" {
			if !reg.ModelAllowed(platform, model) {
				continue
			}
		}
		seen[alias] = struct{}{}
		rows = append(rows, exposedModel{ID: alias, Kind: "alias", Target: target})
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}

type exposedAutoPick struct {
	display string
	raw     string
}

func betterExposedAutoDisplay(next, old exposedAutoPick) bool {
	nextBare := strings.EqualFold(next.raw, next.display)
	oldBare := strings.EqualFold(old.raw, old.display)
	if nextBare != oldBare {
		return nextBare
	}
	if len(next.display) != len(old.display) {
		return len(next.display) < len(old.display)
	}
	return next.display < old.display
}
