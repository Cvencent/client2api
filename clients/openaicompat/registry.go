package openaicompat

// registry.go turns the vendored channel registry into the module's built-in
// provider table.
//
// The registry is a data file rather than another Go map so adding a provider
// is a reviewable JSON change.  Only the format this module can actually
// speak is imported: openai-chat with a concrete base URL.  Native Anthropic
// Messages and OpenAI Responses rows stay out of this package; they belong to
// modules that implement those wires.

import (
	_ "embed"
	"encoding/json"
	"strings"
)

//go:embed registry.json
var registryJSON []byte

type registryRow struct {
	ID      string `json:"id"`
	BaseURL string `json:"baseUrl"`
	Format  string `json:"format"`
	Website string `json:"website"`
	Auth    struct {
		URL string `json:"url"`
	} `json:"auth"`
	Models []struct {
		ID string `json:"id"`
	} `json:"models"`
}

func init() {
	for _, row := range parseRegistry(registryJSON) {
		if _, exists := builtinProviders[row.ID]; !exists {
			builtinProviders[row.ID] = row.BaseURL
		}
		if _, exists := keyPages[row.ID]; !exists && row.KeyPage != "" {
			keyPages[row.ID] = row.KeyPage
		}
		if _, exists := builtinModels[row.ID]; !exists && len(row.Models) > 0 {
			builtinModels[row.ID] = row.Models
		}
	}
}

// parseRegistry decodes and filters the embedded catalogue.  A malformed file
// returns no rows rather than panicking: a bad data update should degrade to
// the hand-written table, not take the module down.
func parseRegistry(raw []byte) []registryProvider {
	if len(raw) == 0 {
		return nil
	}
	var rows []registryRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil
	}
	out := make([]registryProvider, 0, len(rows))
	for _, row := range rows {
		id := strings.ToLower(strings.TrimSpace(row.ID))
		base := strings.TrimRight(strings.TrimSpace(row.BaseURL), "/")
		if id == "" || base == "" || !strings.EqualFold(strings.TrimSpace(row.Format), "openai-chat") {
			continue
		}
		models := make([]string, 0, len(row.Models))
		for _, m := range row.Models {
			if id := strings.TrimSpace(m.ID); id != "" {
				models = append(models, id)
			}
		}
		out = append(out, registryProvider{
			ID:      id,
			BaseURL: base,
			KeyPage: registryKeyPage(row),
			Models:  models,
		})
	}
	return out
}

// registryKeyPage prefers the vendor's API-key page and falls back to its
// website.  Keyless rows (opencode and the local servers) have no auth.url,
// but the panel still needs a useful link for the provider picker.
func registryKeyPage(row registryRow) string {
	if u := strings.TrimSpace(row.Auth.URL); u != "" {
		return u
	}
	return strings.TrimSpace(row.Website)
}

type registryProvider struct {
	ID      string
	BaseURL string
	KeyPage string
	Models  []string
}
