package doubao

import (
	"context"
	"strings"
	"time"

	"client2api/internal/core"
)

// models.go owns the catalogue.  The catalogue is the union of the model menus
// the signed-in accounts are actually showing in the Doubao desktop client:
// "the models in the client" are whatever the server returns in
// action_bar_menu_config, and there is no separate local model API.  Refresh
// re-pulls that menu for every usable account.  The operator `models` config
// and the built-in Ark ids remain only as a fallback for an account that has
// not been probed yet, so the picker is never empty.

// fallbackModelIDs are the ids this module serves when no account menu has
// been read yet.  The Doubao desktop client's own default model selection is
// mode 3 / model item "0" (read out of its signed-in session), so a request
// before the asynchronous refresh still lands on the same model the client
// itself opens with.
func fallbackModelIDs() []string {
	return []string{"default"}
}

// testModel is the model a connectivity probe asks for.
func (c *Client) testModel() string {
	if ids := c.modelIDs(); len(ids) > 0 {
		return ids[0]
	}
	return fallbackModelIDs()[0]
}

// configModelIDs is the operator's override list, de-duplicated.
func (c *Client) configModelIDs() []string {
	return dedupeIDs(c.cfg.Models)
}

// builtinModels renders the fallback catalogue as core.Models.
func (c *Client) builtinModels() []core.Model {
	ids := c.configModelIDs()
	if len(ids) == 0 {
		ids = fallbackModelIDs()
	}
	out := make([]core.Model, 0, len(ids))
	for _, id := range ids {
		out = append(out, core.Model{ID: id, OwnedBy: "doubao"})
	}
	return out
}

// RefreshModels pulls the client model menu for every usable account and
// rebuilds the catalogue as their union.  A failure to read one account keeps
// the entries the others contributed; the last good catalogue is returned
// alongside the first error so a flaky refresh never blanks the picker.
func (c *Client) RefreshModels(ctx context.Context) ([]core.Model, error) {
	accounts := c.store.snapshot()
	var merged []modelRoute
	seen := map[string]bool{}
	var firstErr error
	loaded := 0

	for i := range accounts {
		acc := accounts[i]
		if !acc.selectable(time.Now()) {
			continue
		}
		menu, err := c.pullAccountMenu(ctx, &acc)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		c.setAccountMenu(acc.ID, menu)
		loaded++
		for _, route := range menu.allModelRoutes() {
			lk := strings.ToLower(route.Key)
			if seen[lk] {
				continue
			}
			seen[lk] = true
			merged = append(merged, route)
		}
	}

	models := c.catalogueFromRoutes(merged)
	if len(models) == 0 {
		// No account menu this round: keep the last good catalogue so a flaky
		// refresh never blanks the picker.
		c.modelMu.Lock()
		previous := append([]core.Model(nil), c.catalog...)
		c.modelMu.Unlock()
		if len(previous) > 0 {
			models = previous
		} else {
			models = c.builtinModels()
		}
	}

	c.modelMu.Lock()
	c.catalog = models
	c.catalogAt = time.Now()
	c.modelMu.Unlock()

	if loaded == 0 && firstErr != nil {
		return models, firstErr
	}
	return models, nil
}

// catalogueFromRoutes renders routing entries as core.Models, sorted for a
// stable picker.
func (c *Client) catalogueFromRoutes(routes []modelRoute) []core.Model {
	menu := clientMenu{routes: routes}
	return catalogueFromMenu(menu, "doubao")
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
