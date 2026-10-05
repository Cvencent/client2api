package openaicompat

import (
	"errors"
	"os"
	"path/filepath"

	"client2api/internal/core"
)

// storeVersion is the on-disk schema of accounts.json.  Only providers the
// panel or an import created live here; a provider written into the module
// config is already durable elsewhere and is never copied.
const storeVersion = 1

// storedProvider is one panel-owned provider row.
type storedProvider struct {
	ID       string            `json:"id"`
	Label    string            `json:"label,omitempty"`
	BaseURL  string            `json:"base_url,omitempty"`
	APIKey   string            `json:"api_key"`
	Models   []string          `json:"models,omitempty"`
	Headers  map[string]string `json:"extra_headers,omitempty"`
	Disabled bool              `json:"disabled,omitempty"`
}

// storePath is the module's own credential store.
func (c *Client) storePath() string {
	return filepath.Join(c.deps.DataDir, "accounts.json")
}

// loadProviders reads accounts.json.  A missing or corrupt file is not an
// error: it means "no panel-added provider".
func (c *Client) loadProviders() []storedProvider {
	var store struct {
		Version   int              `json:"version"`
		Providers []storedProvider `json:"providers"`
	}
	if err := core.ReadJSON(c.storePath(), &store); err != nil {
		return nil
	}
	out := make([]storedProvider, 0, len(store.Providers))
	for _, p := range store.Providers {
		if p.ID == "" || p.APIKey == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// saveProviders writes the panel-owned provider rows.  Removing the last one
// removes the file rather than leaving an empty store behind.
func (c *Client) saveProviders(rows []storedProvider) error {
	if len(rows) == 0 {
		if err := os.Remove(c.storePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return core.WriteJSONAtomic(c.storePath(), map[string]any{
		"version":   storeVersion,
		"providers": rows,
	})
}

// storedToConfig converts a stored row into the in-memory provider shape.
func storedToConfig(p storedProvider) ProviderConfig {
	return ProviderConfig{
		ID:           p.ID,
		Label:        p.Label,
		BaseURL:      p.BaseURL,
		APIKey:       p.APIKey,
		Models:       p.Models,
		ExtraHeaders: p.Headers,
		Disabled:     p.Disabled,
	}
}

// configToStored converts a provider into its on-disk row.
func configToStored(p ProviderConfig) storedProvider {
	return storedProvider{
		ID:       p.ID,
		Label:    p.Label,
		BaseURL:  p.BaseURL,
		APIKey:   p.APIKey,
		Models:   p.Models,
		Headers:  p.ExtraHeaders,
		Disabled: p.Disabled,
	}
}
