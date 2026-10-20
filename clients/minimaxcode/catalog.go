package minimaxcode

// Upstream catalogue.
//
// MiniMax Code's desktop client reads its model list from a vendor endpoint
// rather than inventing one locally:
//
//	GET https://agent.minimax.cn/mavis/api/v1/models
//	GET https://agent.minimaxi.com/mavis/api/v1/models   (international build)
//
// The endpoint answers anonymously, so a machine with no credential still gets
// a real catalogue.  Its body is the same configuration object the client
// writes to ~/.minimax/config.yaml:
//
//	{"version":"1.0","ttlSeconds":300,"providers":[{"providerId":"minimax",
//	  "config":{"models":{...},"model_order":[...],"whitelist":[...]}}]}
//
// Two rules govern what this module serves:
//
//   - When the endpoint answers, its `models` map is the whole catalogue.  The
//     `whitelist` is only an allowlist: it still names retired ids (M2.7 on
//     2026-10-20) that have no definition and must not be resurrected.
//   - When the endpoint cannot answer, the local config.yaml and finally the
//     compiled-in table stand in.  The two are never merged with the live list.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	// modelsPath is appended to the catalogue root derived from base_url.
	modelsPath = "/models"

	// modelsFetchTimeout bounds one catalogue call.  It is a small metadata
	// request on a path the panel waits for, not a chat completion.
	modelsFetchTimeout = 20 * time.Second

	// maxModelsBodyBytes caps the catalogue read; the live body is ~2 KB.
	maxModelsBodyBytes = 1 << 20
)

// modelsURL derives the vendor's catalogue endpoint from the configured chat
// base.  The chat base points at the "llm" family root (…/mavis/api/v1/llm, or
// …/llm/v1) while the catalogue hangs off its parent (…/mavis/api/v1/models),
// so the last segment is replaced rather than appended.
func modelsURL(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = defaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	switch {
	case strings.HasSuffix(base, messagesPath):
		base = strings.TrimSuffix(base, messagesPath)
	case strings.HasSuffix(base, "/models"):
		return base
	}
	if strings.HasSuffix(base, "/llm/v1") {
		base = strings.TrimSuffix(base, "/v1")
	}
	base = strings.TrimSuffix(base, "/llm")
	return base + modelsPath
}

// catalogueEnvelope is the vendor config document's outer shape.
type catalogueEnvelope struct {
	Providers []struct {
		ProviderID string `json:"providerId"`
		Config     struct {
			Models     map[string]catalogueModel `json:"models"`
			ModelOrder []string                  `json:"model_order"`
			Whitelist  []string                  `json:"whitelist"`
		} `json:"config"`
	} `json:"providers"`
}

// catalogueModel is one entry of the vendor's `models` map.  Only fields the
// vendor actually publishes are read; an absent limit stays zero rather than
// being guessed.
type catalogueModel struct {
	Name       string `json:"name"`
	Reasoning  bool   `json:"reasoning"`
	Attachment bool   `json:"attachment"`
	Limit      struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
}

// parseUpstreamCatalogue decodes the vendor document into catalogue rows.
//
// Membership comes from `models` alone.  `model_order` (and, failing that,
// `whitelist`) only decides the order; an id named there but absent from
// `models` is skipped, because the vendor published no definition for it.
func parseUpstreamCatalogue(raw []byte) ([]modelInfo, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, errors.New("minimaxcode: the model catalogue is empty")
	}
	var env catalogueEnvelope
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		return nil, fmt.Errorf("minimaxcode: the model catalogue is not JSON: %w", err)
	}
	if len(env.Providers) == 0 {
		return nil, errors.New("minimaxcode: the model catalogue carries no providers")
	}

	// The desktop client's own config is one provider ("minimax"); prefer it
	// when several are present so a future multi-provider answer cannot
	// silently blend catalogues.
	cfg := env.Providers[0].Config
	for _, p := range env.Providers {
		if strings.EqualFold(strings.TrimSpace(p.ProviderID), "minimax") {
			cfg = p.Config
			break
		}
	}
	if len(cfg.Models) == 0 {
		return nil, errors.New("minimaxcode: the model catalogue carries no models")
	}

	seen := make(map[string]bool, len(cfg.Models))
	out := make([]modelInfo, 0, len(cfg.Models))
	appendID := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		m, ok := cfg.Models[id]
		if !ok {
			// Named by the whitelist only: the vendor no longer defines it.
			return
		}
		seen[id] = true
		out = append(out, modelInfo{
			ID:      id,
			Name:    strings.TrimSpace(m.Name),
			Context: m.Limit.Context,
			Output:  m.Limit.Output,
			Vision:  m.Attachment,
			Reason:  m.Reasoning,
		})
	}

	order := cfg.ModelOrder
	if len(order) == 0 {
		order = cfg.Whitelist
	}
	for _, id := range order {
		appendID(id)
	}
	// Anything the vendor defines but did not order still belongs in the
	// catalogue; sort it so repeated calls are stable.
	rest := make([]string, 0, len(cfg.Models))
	for id := range cfg.Models {
		if !seen[id] {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	for _, id := range rest {
		appendID(id)
	}
	return out, nil
}

// fetchUpstreamCatalogue performs one GET against the vendor's catalogue.
func (c *Client) fetchUpstreamCatalogue(ctx context.Context) ([]modelInfo, error) {
	endpoint := modelsURL(c.baseURL(nil))
	reqCtx, cancel := context.WithTimeout(ctx, modelsFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("minimaxcode: building the catalogue request failed: %w", err)
	}
	// The endpoint answers without a credential, so none is sent: a parked or
	// expired token would otherwise turn a public catalogue read into a 401 and
	// hide the models behind an unrelated auth problem.
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("minimaxcode: the catalogue request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("minimaxcode: reading the model catalogue failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("minimaxcode: the model catalogue answered HTTP %d", resp.StatusCode)
	}
	return parseUpstreamCatalogue(raw)
}
