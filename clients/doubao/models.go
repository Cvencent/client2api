package doubao

import (
	"context"
	"strings"
	"time"

	"client2api/internal/core"
)

// models.go owns the catalogue.  Doubao does not expose a public model-list
// endpoint to this web app, so the catalogue is the platform's own model ids
// (the same ids the Volcengine Ark platform publishes) plus anything the
// operator adds under `models`.  RefreshModels re-reads config and clears the
// cache, which is what the panel's refresh button calls.

// defaultModelIDs are the model ids this module serves out of the box.  They
// match the ids the Ark platform uses, which is what the Doubao backend accepts
// as `model` on a completion.
func defaultModelIDs() []string {
	return []string{
		"doubao-seed-2.1-pro",
		"doubao-seed-2.1-turbo",
		"doubao-seed-2.0-pro",
		"doubao-seed-2.0-lite",
	}
}

// testModel is the model a connectivity probe asks for.  It is the vendor's
// current flagship, which is what a probe wants.
func (c *Client) testModel() string {
	if ids := c.configModelIDs(); len(ids) > 0 {
		return ids[0]
	}
	return defaultModelIDs()[0]
}

// configModelIDs is the operator's override list, de-duplicated.
func (c *Client) configModelIDs() []string {
	return dedupeIDs(c.cfg.Models)
}

// builtinModels renders the catalogue as core.Models.
func (c *Client) builtinModels() []core.Model {
	ids := c.configModelIDs()
	if len(ids) == 0 {
		ids = defaultModelIDs()
	}
	out := make([]core.Model, 0, len(ids))
	for _, id := range ids {
		out = append(out, core.Model{ID: id, OwnedBy: "doubao"})
	}
	return out
}

// RefreshModels clears the cache and re-reads the configured catalogue.
func (c *Client) RefreshModels(ctx context.Context) ([]core.Model, error) {
	models := c.builtinModels()
	c.modelMu.Lock()
	c.catalog = models
	c.catalogAt = time.Now()
	c.modelMu.Unlock()
	return models, nil
}

// refreshModelsAsync refreshes the cache off the request path.
func (c *Client) refreshModelsAsync() {
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.probeTimeout())
	go func() {
		defer cancel()
		_, _ = c.RefreshModels(ctx)
	}()
}

// dedupeIDs trims, drops blanks and removes duplicates, preserving order.
func dedupeIDs(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
