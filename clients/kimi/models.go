package kimi

// The vendor's real model list.
//
// Models() used to answer only from a hard-coded catalogue, so the panel's
// picker was a guess: the ids came from docs and from the desktop app's model
// cache, and nothing could tell whether the signed-in account could actually
// use them.  The vendor does serve a real list -- an unauthenticated probe of
//
//	GET https://api.kimi.com/coding/v1/models
//
// answers HTTP 401 with the standard OpenAI error envelope
// ({"error":{"message":"Invalid Authentication","type":
// "invalid_authentication_error"}}), which is how we know the endpoint exists
// and only needs the account's token.
//
// So this file adds that endpoint as the *preferred* source and keeps the
// built-in catalogue as the fallback, so the module still works with no
// credential at all.  Three rules shape the design:
//
//   - Models() is called on every panel refresh, so it must not reach the
//     network every time.  It answers from a TTL cache (modelsCacheTTL) and
//     only asks upstream when that cache is empty or stale.
//
//   - A failed refresh must never empty the catalogue.  RefreshModels returns
//     the last good list (or the module's baseline catalogue) *alongside* the
//     error, so one flaky refresh cannot blank the model picker.
//
//   - No credential means no request.  With nothing to authenticate with the
//     module answers from the baseline catalogue and never touches the
//     network; the test suite asserts the vendor sees zero requests.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"client2api/internal/core"
)

// Client implements the core's optional "re-read my catalogue from the vendor"
// capability.  This is what makes the panel offer a re-fetch button, so the
// assertion is deliberate: drop it and the button disappears.
var _ core.ModelRefresher = (*Client)(nil)

const (
	// modelsPath is appended to apiBase(tok), which already carries the
	// regional root and the version segment (…/coding/v1).
	modelsPath = "/models"

	// modelsCacheTTL is how long a fetched list is trusted.  The vendor's
	// catalogue changes on the order of weeks while the panel refreshes on the
	// order of seconds, so a few minutes is generous; the panel's explicit
	// re-fetch button (RefreshModels) bypasses it entirely.
	modelsCacheTTL = 5 * time.Minute

	// modelsFetchTimeout bounds one GET /models.  Deliberately much shorter
	// than Config.TimeoutSeconds (600s): this is a small metadata call on a
	// path the panel waits for, not a chat completion.
	modelsFetchTimeout = 8 * time.Second

	// maxModelsBodyBytes caps how much of the response body is read.  A model
	// list is a few kilobytes; anything past this is not a model list.
	maxModelsBodyBytes = 1 << 20

	// modelsSourceUpstream tags entries that came from the vendor, mirroring
	// the "source" key builtinCatalog() sets.
	modelsSourceUpstream = "upstream"
)

// ---------------------------------------------------------------------------
// Credential selection
// ---------------------------------------------------------------------------

// modelToken returns the token to present to GET /models, in the order this
// module trusts: its own store first (a panel login), then the CLI's own
// credentials file.
//
// Expiry is deliberately not consulted.  A stored expiry is this module's
// guess about the vendor's state, and the vendor is the authority on whether a
// token still works; throwing away a token the server would have accepted
// would turn a working account into an empty picker for no reason.  A
// genuinely dead token 401s and the baseline catalogue is used instead.
func (c *Client) modelToken() (storedToken, bool) {
	if tok, ok := c.loadToken(); ok {
		return tok, true
	}
	return c.credentialFileToken()
}

// modelTokenForUse returns the credential to authenticate a catalogue request
// with, renewing a grant that is at or near its stated expiry first.
//
// A renewal failure is not fatal: the stored token is still returned and the
// request still goes out, because the vendor is the authority on whether it
// works and a 401 already falls back to the baseline catalogue.  Only the log
// records that the credential is on its way out.
func (c *Client) modelTokenForUse(ctx context.Context) (storedToken, bool) {
	tok, ok, err := c.freshToken(ctx)
	if err != nil {
		c.deps.Log("kimi: the stored credential could not be renewed before a catalogue request: %v", err)
	}
	if ok {
		return tok, true
	}
	return c.modelToken()
}

// credentialFileToken reads an access token out of a CLI credentials file
// (catalog.go's credentialFileCandidates, or Config.CredentialFiles).
//
// expires_at is ignored on purpose: a token the vendor still accepts must not
// be discarded on a guess.
func (c *Client) credentialFileToken() (storedToken, bool) {
	files := c.cfg.CredentialFiles
	if len(files) == 0 {
		files = credentialFileCandidates()
	}
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		// Only a real bearer access token is used.  api_key / id_token are
		// deliberately not consulted: they are credentials of a different
		// kind, and sending one as a Bearer token would be a guess.
		for _, key := range []string{"access_token", "accessToken"} {
			raw, ok := m[key]
			if !ok {
				continue
			}
			var s string
			if json.Unmarshal(raw, &s) != nil {
				continue
			}
			if s = strings.TrimSpace(s); s == "" {
				continue
			}
			// A CLI credential does not record its issuer, so apiBase() falls
			// back to the mainland root -- the one the CLI itself logs in to.
			return storedToken{AccessToken: s}, true
		}
	}
	return storedToken{}, false
}

// ---------------------------------------------------------------------------
// Cache
// ---------------------------------------------------------------------------

// baseline is the catalogue the module answers with when it cannot ask the
// vendor: Config.Models when the operator set one, otherwise builtinCatalog()
// (via catalog()).  It is never empty.
func (c *Client) baseline() []core.Model { return cloneModels(c.cat) }

// cloneModels copies a list so a caller can never mutate the module's own
// catalogue (or its cache) through the returned slice.
func cloneModels(in []core.Model) []core.Model {
	if in == nil {
		return nil
	}
	out := make([]core.Model, len(in))
	for i, m := range in {
		out[i] = m
		if m.Extra != nil {
			extra := make(map[string]any, len(m.Extra))
			for k, v := range m.Extra {
				extra[k] = v
			}
			out[i].Extra = extra
		}
	}
	return out
}

// lastGoodLocked returns the newest list worth showing.  modelsMu must be held.
func (c *Client) lastGoodLocked() []core.Model {
	if len(c.modelsList) > 0 {
		return cloneModels(c.modelsList)
	}
	return c.baseline()
}

// ModelMaxOutputTokens implements core.ModelLimitsProvider.  When the caller
// omits max_tokens the gateway asks what the vendor advertises for this model,
// instead of leaving the request without a budget.
//
// kimi merges internal/modelmeta into its catalogue, so the number is present
// even when the vendor's own list is unreachable -- that is what the baseline
// catalogue is for.  It reads the cache under the same lock Models uses and
// never fetches, because this runs inside a chat request: a cold cache answers
// "cannot say" rather than turning a chat turn into a metadata round trip.
func (c *Client) ModelMaxOutputTokens(ctx context.Context, model string) (int, bool) {
	c.modelsMu.Lock()
	models := c.lastGoodLocked()
	c.modelsMu.Unlock()
	return core.OutputLimitFor(models, model)
}

// claimFetch reports whether this caller should be the one to ask upstream,
// and the cached answer when it should not.  It records the attempt *before*
// the network call so that concurrent callers do not all fire at once and so
// that a refusing upstream is not hammered on every panel refresh.
func (c *Client) claimFetch() (bool, []core.Model) {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()

	if len(c.modelsList) > 0 && time.Since(c.modelsAt) < modelsCacheTTL {
		return false, cloneModels(c.modelsList)
	}
	if !c.modelsTried.IsZero() && time.Since(c.modelsTried) < modelsCacheTTL {
		// A recent attempt failed (or is still in flight): keep the last good
		// list rather than retrying inside the window.
		return false, c.lastGoodLocked()
	}
	c.modelsTried = time.Now()
	return true, nil
}

// listModels backs Client.Models.
func (c *Client) listModels(ctx context.Context) ([]core.Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	// Fast path: a fresh list needs neither a credential read nor a request.
	c.modelsMu.Lock()
	if len(c.modelsList) > 0 && time.Since(c.modelsAt) < modelsCacheTTL {
		out := cloneModels(c.modelsList)
		c.modelsMu.Unlock()
		return out, nil
	}
	c.modelsMu.Unlock()

	tok, ok := c.modelTokenForUse(ctx)
	if !ok {
		// Nothing to authenticate with.  This deliberately does not claim the
		// fetch window: a token that appears later (a panel login) is picked
		// up on the very next call.
		return c.baseline(), nil
	}

	claimed, cached := c.claimFetch()
	if !claimed {
		return cached, nil
	}

	// Models is called on every panel refresh, so a failure here is logged and
	// swallowed: the caller asked what the module can serve, and the honest
	// answer is still "this list".  The panel's explicit refresh button
	// (RefreshModels) is where an error is reported to a human.
	list, err := c.fetchAndStore(ctx, tok)
	if err != nil {
		c.deps.Log("kimi: could not refresh the model list from the vendor, keeping the last known list: %v", err)
	}
	return list, nil
}

// RefreshModels implements core.ModelRefresher: it bypasses the TTL cache and
// asks the vendor again.
//
// A failed refresh must never empty the catalogue, so the returned slice is
// always usable: the last good list, or the module's baseline catalogue when
// nothing has ever been fetched.  err is non-nil only when upstream was
// actually asked and the answer was unusable; having no credential at all is
// not an error, it is simply a module that answers offline.
func (c *Client) RefreshModels(ctx context.Context) ([]core.Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	tok, ok := c.modelTokenForUse(ctx)
	if !ok {
		return c.baseline(), nil
	}

	// Claim the window so a concurrent Models() does not duplicate this call.
	c.modelsMu.Lock()
	c.modelsTried = time.Now()
	c.modelsMu.Unlock()

	return c.fetchAndStore(ctx, tok)
}

// fetchAndStore performs the request and records the outcome.  It always
// returns a usable list: the fetched one on success, the last good one (or the
// baseline) on failure.
func (c *Client) fetchAndStore(ctx context.Context, tok storedToken) ([]core.Model, error) {
	fetched, err := c.fetchUpstreamModels(ctx, tok)
	if err == nil && len(fetched) == 0 {
		// A 200 with an empty catalogue is not something to trust: treat it
		// like any other unusable answer.
		err = fmt.Errorf("kimi: the vendor returned an empty model list")
	}

	c.modelsMu.Lock()
	c.modelsTried = time.Now()
	if err == nil {
		c.modelsList = fetched
		c.modelsAt = time.Now()
	}
	fallback := c.lastGoodLocked()
	c.modelsMu.Unlock()

	if err != nil {
		return fallback, scrubModelError(err, tok)
	}
	return cloneModels(fetched), nil
}

// ---------------------------------------------------------------------------
// Fetch
// ---------------------------------------------------------------------------

// fetchUpstreamModels performs one GET {apiBase(tok)}/models.
func (c *Client) fetchUpstreamModels(ctx context.Context, tok storedToken) ([]core.Model, error) {
	endpoint := c.apiBase(tok) + modelsPath

	ctx, cancel := context.WithTimeout(ctx, modelsFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("kimi: building the model-list request failed: %w", err)
	}
	// The token travels in the header only; it is never part of the URL, so no
	// error string built from endpoint can contain it.
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	// The CLI asks for this list with the same generated SDK it uses for chat,
	// so it carries the same identity block and the same accept.
	applyIdentityHeaders(req.Header, c.identity(), true)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("kimi: the request to %s failed: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("kimi: reading the model list from %s failed: %w", endpoint, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kimi: %s returned HTTP %d%s", endpoint, resp.StatusCode, upstreamDetail(body))
	}

	models, err := parseModelList(body)
	if err != nil {
		return nil, fmt.Errorf("kimi: %s: %w", endpoint, err)
	}
	return models, nil
}

// ---------------------------------------------------------------------------
// Response parsing
// ---------------------------------------------------------------------------

// modelWire is one entry of the vendor's list.  Only fields we are sure about
// are read; anything else is ignored rather than guessed at.
type modelWire struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	DisplayName     string          `json:"display_name"`
	ContextLength   json.RawMessage `json:"context_length"`
	ContextWindow   json.RawMessage `json:"context_window"`
	MaxOutputTokens json.RawMessage `json:"max_output_tokens"`
	MaxOutput       json.RawMessage `json:"max_output"`
}

// model converts one wire entry.  The bool is false when the entry carries no
// usable id.
func (w modelWire) model() (core.Model, bool) {
	m, ok := modelFromID(w.ID)
	if !ok {
		return core.Model{}, false
	}
	if n, ok := wireInt(w.ContextLength); ok {
		m.Extra["context_length"] = n
	} else if n, ok := wireInt(w.ContextWindow); ok {
		m.Extra["context_length"] = n
	}
	if n, ok := wireInt(w.MaxOutputTokens); ok {
		m.Extra["max_output_tokens"] = n
	} else if n, ok := wireInt(w.MaxOutput); ok {
		m.Extra["max_output_tokens"] = n
	}
	if name := firstNonEmpty(strings.TrimSpace(w.DisplayName), strings.TrimSpace(w.Name)); name != "" && name != m.ID {
		m.Extra["display_name"] = name
	}
	return m, true
}

// modelFromID builds a catalog entry for one id.
//
// OwnedBy is ownerKimi ("moonshot"), the same value builtinCatalog() sets: the
// panel groups and labels models by it, and having the upstream half of the
// list disagree with the fallback half would make the picker look like two
// different vendors.  The vendor's own owned_by string is not echoed anywhere,
// because where it differs from ownerKimi it is unclear which of the two the
// panel should trust.
func modelFromID(id string) (core.Model, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return core.Model{}, false
	}
	return core.Model{
		ID:      id,
		OwnedBy: ownerKimi,
		Extra:   map[string]any{"source": modelsSourceUpstream},
	}, true
}

// wireInt reads a positive integer out of a raw JSON field.  A missing, null,
// non-numeric or nonsensical value is reported as "not present" rather than
// guessed at.
func wireInt(raw json.RawMessage) (int, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	if n <= 0 || n != math.Trunc(n) || n > math.MaxInt32 {
		return 0, false
	}
	return int(n), true
}

// parseModelList accepts the shapes the vendor is known (or reasonably
// expected) to use:
//
//	{"data":[{"id":"kimi-k2","owned_by":"moonshot"}, …]}   OpenAI
//	{"models":[{…}]}                                        sibling key
//	{"data":["kimi-k2", …]}                                 bare ids
//	[{"id":"…"}]                                            bare array
//
// Anything else is an error, which the caller turns into "keep the baseline
// catalogue".  In particular an object where a list belongs is NOT mined for
// keys: doing so would invent model ids out of metadata fields.
func parseModelList(body []byte) ([]core.Model, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errors.New("the model list was empty")
	}
	if trimmed[0] == '[' {
		return decodeModelArray(trimmed)
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Models json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil, fmt.Errorf("the model list was not JSON: %w", err)
	}

	var firstErr error
	for _, raw := range []json.RawMessage{envelope.Data, envelope.Models} {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		if raw[0] != '[' {
			if firstErr == nil {
				firstErr = errors.New(`"data"/"models" was not an array`)
			}
			continue
		}
		out, err := decodeModelArray(raw)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, errors.New("the response carried no usable models")
}

// decodeModelArray decodes one array of entries, each an object or a bare id
// string.  A malformed entry is dropped rather than failing the whole list; an
// entry with no id at all carries nothing to report.
func decodeModelArray(raw []byte) ([]core.Model, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("the model list was not an array: %w", err)
	}
	out := make([]core.Model, 0, len(entries))
	for _, entry := range entries {
		entry = bytes.TrimSpace(entry)
		if len(entry) == 0 {
			continue
		}
		switch entry[0] {
		case '"':
			var id string
			if json.Unmarshal(entry, &id) != nil {
				continue
			}
			if m, ok := modelFromID(id); ok {
				out = append(out, m)
			}
		case '{':
			var w modelWire
			if json.Unmarshal(entry, &w) != nil {
				continue
			}
			if m, ok := w.model(); ok {
				out = append(out, m)
			}
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Secret scrubbing
// ---------------------------------------------------------------------------

// scrubModelError makes an error safe to hand to a caller or a log line: every
// message is prefixed "kimi: " and run through the module's scrubber, and the
// exact token used for the call is removed as well.
//
// The second step matters because the vendor can echo a request back in an
// error body, and an opaque access token has no recognisable shape -- a JWT or
// "Bearer …" pattern would be caught by redactSecrets, a bare random string
// would not.
func scrubModelError(err error, tok storedToken) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	changed := false

	if t := strings.TrimSpace(tok.AccessToken); len(t) >= 8 && strings.Contains(msg, t) {
		msg = strings.ReplaceAll(msg, t, core.MaskSecret(t))
		changed = true
	}
	if scrubbed := redactSecrets(msg); scrubbed != msg {
		msg = scrubbed
		changed = true
	}
	if !changed {
		// Nothing to scrub: keep the original error, and with it its wrapping
		// chain (errors.Is/As still work for callers that care).
		return err
	}
	if !strings.HasPrefix(msg, "kimi: ") {
		msg = "kimi: " + msg
	}
	return errors.New(msg)
}
