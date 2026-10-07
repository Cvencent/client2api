package tabbit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Panel account management.
//
// tabbit has no vendor credential to manage.  The thing an operator actually
// needs to add, test and remove is a SIDECAR ENDPOINT: a base URL plus the
// local bearer it accepts.  So this file stores endpoints rather than tokens,
// and every operation here is really a question about reachability.  Saying
// "no accounts" for tabbit would be technically true and completely useless.
// ---------------------------------------------------------------------------

const (
	accountsFileName = "accounts.json"

	epStateReady   = "ready"
	epStateInvalid = "invalid"
	epStateUnknown = "unknown"

	epOriginStored = "stored"
	epOriginPanel  = "panel"

	// testPrompt is the smallest request that proves a sidecar is not just
	// answering /health but can actually reach its browser.
	testPrompt = "Reply with the single word: pong"
)

var (
	_ core.AccountManager     = (*Client)(nil)
	_ core.CredentialImporter = (*Client)(nil)
)

// storedEndpoint is one account the operator added through the panel.
//
// Two kinds share this record because the panel has one add-account form:
// a "sidecar" account names a local tabbit2api bridge, and a "web-token"
// account carries the Tabbit browser's own session cookie so this module can
// talk to the vendor directly.  An empty Kind means "sidecar", which is what
// every account written before kinds existed is.
type storedEndpoint struct {
	ID      string `json:"id"`
	Kind    string `json:"kind,omitempty"`
	Label   string `json:"label,omitempty"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key,omitempty"`
	// Token is a web-token account's session cookie.  It never leaves this
	// process: Accounts() reports only whether it is present.
	Token   string `json:"token,omitempty"`
	WebHost string `json:"web_host,omitempty"`
	Enabled bool   `json:"enabled"`
	AddedAt string `json:"added_at,omitempty"`
}

// epKind is an endpoint's account kind, with the empty value read as "sidecar".
func epKind(ep storedEndpoint) string {
	if k := strings.ToLower(strings.TrimSpace(ep.Kind)); k != "" {
		return k
	}
	return kindSidecar
}

// shortUID renders an account uid for a label without printing all of it.
func shortUID(uid string) string {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return "unknown account"
	}
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

type endpointFile struct {
	Version   int              `json:"version"`
	Endpoints []storedEndpoint `json:"endpoints"`
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

// loadEndpoints reads the panel store.  An absent or unreadable file is not an
// error: it means nothing has been added yet.
func loadEndpoints(dir string) *endpointFile {
	f := &endpointFile{Version: 1}
	if strings.TrimSpace(dir) == "" {
		return f
	}
	b, err := os.ReadFile(filepath.Join(dir, accountsFileName))
	if err != nil {
		return f
	}
	var got endpointFile
	if err := json.Unmarshal(b, &got); err != nil {
		return f
	}
	if len(got.Endpoints) > 0 {
		f.Endpoints = got.Endpoints
	}
	if got.Version > 0 {
		f.Version = got.Version
	}
	return f
}

// saveLocked writes the store atomically, so a crash mid-write cannot leave a
// half-written file that the next start would refuse to parse.  Callers hold
// acctMu.
func (c *Client) saveEndpointsLocked() error {
	dir := strings.TrimSpace(c.deps.DataDir)
	if dir == "" {
		return errors.New("no data directory configured")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if c.acct == nil {
		c.acct = &endpointFile{Version: 1}
	}
	blob, err := json.MarshalIndent(c.acct, "", "  ")
	if err != nil {
		return err
	}
	blob = append(blob, '\n')
	path := filepath.Join(dir, accountsFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// mutateEndpoints applies fn under the lock and persists only when fn reports a
// change, so the file and the in-memory copy cannot drift apart.
func (c *Client) mutateEndpoints(fn func(f *endpointFile) bool) (bool, error) {
	c.acctMu.Lock()
	defer c.acctMu.Unlock()
	if c.acct == nil {
		c.acct = &endpointFile{Version: 1}
	}
	if !fn(c.acct) {
		return false, nil
	}
	return true, c.saveEndpointsLocked()
}

// endpointsSnapshot copies the store out from under the lock.
func (c *Client) endpointsSnapshot() []storedEndpoint {
	c.acctMu.Lock()
	defer c.acctMu.Unlock()
	if c.acct == nil {
		return nil
	}
	return append([]storedEndpoint(nil), c.acct.Endpoints...)
}

func (f *endpointFile) index(id string) int {
	for i := range f.Endpoints {
		if f.Endpoints[i].ID == id {
			return i
		}
	}
	return -1
}

// firstEnabledStored returns the sidecar endpoint the module should prefer, if
// the operator added one.  Web-token accounts are skipped: their BaseURL is the
// vendor's own host, which is emphatically not a sidecar to probe.
func (c *Client) firstEnabledStored() (storedEndpoint, bool) {
	eps := c.endpointsSnapshot()
	best, found := 0, false
	for _, ep := range eps {
		if epKind(ep) != kindSidecar || !ep.Enabled || strings.TrimSpace(ep.BaseURL) == "" {
			continue
		}
		prio := core.AccountPriority("tabbit", ep.ID)
		if !found || prio < best {
			best, found = prio, true
		}
	}
	if !found {
		return storedEndpoint{}, false
	}
	for _, ep := range eps {
		if epKind(ep) != kindSidecar {
			continue
		}
		if ep.Enabled && strings.TrimSpace(ep.BaseURL) != "" {
			if core.AccountPriority("tabbit", ep.ID) != best {
				continue
			}
			return ep, true
		}
	}
	return storedEndpoint{}, false
}

// ---------------------------------------------------------------------------
// Endpoint identity
// ---------------------------------------------------------------------------

// normalizeBaseURL accepts what a human types ("127.0.0.1:50124") as well as a
// full URL, and returns one canonical form.  The canonical form is also the
// account id, because for an endpoint the URL *is* the identity.
func normalizeBaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("base_url is required")
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("base_url is not a URL: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("base_url scheme %q is not supported (use http or https)", u.Scheme)
	}
	if strings.TrimSpace(u.Host) == "" {
		return "", errors.New("base_url has no host")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	u.User = nil
	return strings.TrimRight(u.String(), "/"), nil
}

func endpointLabel(ep storedEndpoint) string {
	if strings.TrimSpace(ep.Label) != "" {
		return ep.Label
	}
	if epKind(ep) == kindWebToken {
		claims, _ := parseWebToken(ep.Token)
		return webCookieHost + " · " + shortUID(claims.Sub)
	}
	return ep.BaseURL
}

// endpointLocation turns a stored endpoint into the module's internal location.
func (c *Client) endpointLocation(ep storedEndpoint) location {
	key := strings.TrimSpace(ep.APIKey)
	keySource := "panel"
	if key == "" {
		key, keySource = defaultAPIKey, "builtin"
	}
	return location{
		baseURL:   ep.BaseURL,
		apiKey:    key,
		keySource: keySource,
		source:    epOriginPanel,
	}
}

// ---------------------------------------------------------------------------
// AccountManager
// ---------------------------------------------------------------------------

// AccountFields is the "add account" form.  One form serves both kinds: pick
// "sidecar" and fill in a URL, or pick "web-token" and paste the browser's
// session cookie (or press 导入凭据 and let this module read it out of the
// running Tabbit browser).
func (c *Client) AccountFields(ctx context.Context) []core.FieldSpec {
	return []core.FieldSpec{
		{
			Key: "kind", Label: "Kind", Type: "select", Default: kindSidecar,
			Options: []string{kindSidecar, kindWebToken},
			Help: "sidecar = a local tabbit2api bridge this module talks to. " +
				"web-token = the Tabbit browser's own session cookie, which lets this module call web.tabbit.com directly.",
		},
		{
			Key: "token", Label: "Web session cookie", Type: "password",
			Placeholder: "eyJhbGciOi…",
			Help: "For kind web-token: the value of the `token` cookie from a signed-in web.tabbit.com tab. " +
				"Leave it blank and use 导入凭据 to read it out of the running Tabbit browser instead. It is never returned to the panel.",
		},
		{
			Key: "base_url", Label: "Sidecar URL", Type: "text",
			Placeholder: firstCandidate(),
			Help:        "For kind sidecar: where the tabbit2api sidecar listens. Defaults to " + firstCandidate() + ". A bare host:port is accepted and assumed to be http.",
		},
		{
			Key: "api_key", Label: "Local bearer", Type: "password",
			Placeholder: defaultAPIKey,
			Help:        "The sidecar's local bearer. Leave blank for the documented default (" + defaultAPIKey + "). It is never returned to the panel.",
		},
		{
			Key: "web_host", Label: "Browser host", Type: "text",
			Placeholder: webCookieHost,
			Help:        "The Tabbit web host the browser login opens, and the host the sidecar is expected to drive. Defaults to web.tabbit.com; a mismatch is reported as host drift in the account note.",
		},
		{
			Key: "label", Label: "Label", Type: "text",
			Placeholder: "local sidecar",
		},
	}
}

// Accounts lists every endpoint this module can see: the ones the operator
// added, plus whatever config, environment, the hand-off file or the default
// probe resolved.  A disabled endpoint stays listed so it can be switched back
// on.  No secret is ever included.
func (c *Client) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	stored := c.endpointsSnapshot()
	active, _ := c.healthSnapshot()

	out := make([]core.AccountRecord, 0, len(stored))

	for _, ep := range stored {
		if ep.ID == "" {
			continue
		}
		rec := core.AccountRecord{
			ID:      ep.ID,
			Label:   endpointLabel(ep),
			Enabled: ep.Enabled,
			State:   epStateUnknown,
			Note:    "",
			Fields: map[string]any{
				"origin":       epOriginStored,
				"managed":      true,
				"kind":         kindSidecar,
				"base_url":     ep.BaseURL,
				"has_api_key":  strings.TrimSpace(ep.APIKey) != "",
				"web_host":     ep.WebHost,
				"added_at":     ep.AddedAt,
				"key_source":   "panel",
				"sidecar_hint": "start the sidecar yourself; this module only talks to it",
			},
		}
		if epKind(ep) == kindWebToken {
			c.fillWebRecord(&rec, ep)
			out = append(out, rec)
			continue
		}
		if !ep.Enabled {
			rec.Note = "disabled by operator"
		} else if active.baseURL == ep.BaseURL && !active.at.IsZero() {
			if active.ok {
				rec.State = epStateReady
				rec.Note = sidecarNote(active)
			} else {
				rec.State = epStateInvalid
				rec.Note = truncate(active.err, 200)
			}
		}
		out = append(out, rec)
	}

	// An endpoint this module merely RESOLVES — clients.tabbit.base_url, the
	// environment, the hand-off file, or the built-in default — is deliberately
	// not listed here.  This table is the panel's editing surface: every row
	// carries 删除 / 停用 buttons, and RemoveAccount and SetAccountEnabled both
	// refuse to touch anything the panel did not add.  Listing a resolved
	// default therefore produced a "account" whose only two buttons could do
	// nothing but fail.  Where the gateway would actually talk to, and whether
	// it answers, is reported by Status().Detail (and by /v1/status).
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func sidecarNote(st healthState) string {
	parts := make([]string, 0, 3)
	if st.version != "" {
		parts = append(parts, "version "+st.version)
	}
	if st.models > 0 {
		parts = append(parts, fmt.Sprintf("%d models", st.models))
	}
	if st.host != "" {
		parts = append(parts, "drives "+st.host)
	}
	if len(parts) == 0 {
		return "healthy"
	}
	return strings.Join(parts, "; ")
}

// fillWebRecord describes a web-token account: its fields, its state and, when
// the cookie carries one, its expiry.  The token itself is never included.
func (c *Client) fillWebRecord(rec *core.AccountRecord, ep storedEndpoint) {
	claims, _ := parseWebToken(ep.Token)
	exp := webTokenExpiry(claims)
	if !exp.IsZero() {
		rec.ExpiresAt = exp.UTC().Format(time.RFC3339)
	}
	rec.Fields = map[string]any{
		"origin":     epOriginStored,
		"managed":    true,
		"kind":       kindWebToken,
		"base_url":   ep.BaseURL,
		"web_host":   firstNonEmpty(ep.WebHost, webCookieHost),
		"has_token":  strings.TrimSpace(ep.Token) != "",
		"uid":        strings.TrimSpace(claims.Sub),
		"expires":    rec.ExpiresAt,
		"added_at":   ep.AddedAt,
		"key_source": "panel",
		"web_hint": "this module calls web.tabbit.com directly with the browser's cookie; " +
			"no sidecar is involved",
	}

	verdict, seen := c.webVerdictFor(ep.ID)
	// Status() (the badge in the account pool) and Accounts() (the table) must
	// answer "is this cookie ready?" the same way; the two views used to read
	// the same verdict with different rules, so one timed-out status probe could
	// paint the badge red while this table still showed the row as ready.
	trusted := webVerdictTrusted(verdict, seen)
	// A caller that already has something to say (AddAccount) keeps its words.
	note := strings.TrimSpace(rec.Note)
	rec.Note = ""
	switch {
	case !ep.Enabled:
		rec.State = epStateUnknown
		rec.Note = "disabled by operator"
	case !exp.IsZero() && time.Now().After(exp):
		rec.State = epStateInvalid
		rec.Note = "the cookie expired at " + exp.UTC().Format(time.RFC3339) +
			"; sign in again in the Tabbit browser and press 导入凭据"
	case trusted && verdict.err != "":
		rec.State = epStateInvalid
		rec.Note = verdict.err
	case trusted:
		rec.State = epStateReady
		rec.Note = webReadyNote(verdict)
	case seen:
		rec.State = epStateUnknown
		rec.Note = "the last check succeeded more than " + webTrustWindow.String() +
			" ago; press Test to check the cookie again"
	default:
		rec.State = epStateUnknown
		rec.Note = "not checked yet; press Test, or press 导入凭据 to read the cookie out of the Tabbit browser"
	}
	if note != "" {
		rec.Note = note
	}
}

// webReadyNote renders the last successful verification.
func webReadyNote(v webVerdict) string {
	note := fmt.Sprintf("web.tabbit.com answered; %d models in the vendor catalog", v.models)
	if !v.at.IsZero() {
		note += " (checked " + v.at.UTC().Format(time.RFC3339) + ")"
	}
	return note
}

// AddAccount stores one account, dispatching on the spec's "kind" field.  The
// id is the canonical URL for a sidecar and the account uid for a web token, so
// adding the same account twice updates it instead of duplicating it.
func (c *Client) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	switch kind := accountKind(spec); kind {
	case kindSidecar:
		return c.addSidecarAccount(spec)
	case kindWebToken:
		return c.addWebAccount(spec)
	default:
		return core.AccountRecord{}, fmt.Errorf("unknown account kind %q (want %q or %q)",
			kind, kindSidecar, kindWebToken)
	}
}

// accountKind reads the kind out of a spec, defaulting to "sidecar".
func accountKind(spec core.AccountSpec) string {
	if k := strings.ToLower(strings.TrimSpace(spec.Field("kind"))); k != "" {
		return k
	}
	return kindSidecar
}

func (c *Client) addSidecarAccount(spec core.AccountSpec) (core.AccountRecord, error) {
	base, err := normalizeBaseURL(spec.Field("base_url"))
	if err != nil {
		return core.AccountRecord{}, err
	}
	label := firstNonEmpty(spec.Field("label"), spec.Label)

	ep := storedEndpoint{
		ID:      base,
		Kind:    kindSidecar,
		Label:   label,
		BaseURL: base,
		APIKey:  strings.TrimSpace(spec.Field("api_key")),
		WebHost: strings.TrimSpace(spec.Field("web_host")),
		Enabled: spec.EnabledOr(true),
		AddedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if spec.ID != "" {
		ep.ID = spec.ID
	}

	if err := c.upsertEndpoint(ep); err != nil {
		return core.AccountRecord{}, fmt.Errorf("persist endpoint: %w", err)
	}
	return c.accountRecord(ep, "added; press Test to probe it"), nil
}

// addWebAccount stores the Tabbit browser's session cookie.  The token is
// checked locally (is it a JWT, has it expired) before it is written; whether
// the vendor still accepts it is what Test answers.
func (c *Client) addWebAccount(spec core.AccountSpec) (core.AccountRecord, error) {
	token := strings.TrimSpace(spec.Field("token"))
	if token == "" {
		return core.AccountRecord{}, errors.New(
			"token is required for a web-token account: paste the web.tabbit.com `token` cookie, " +
				"or press 导入凭据 to read it out of the running Tabbit browser")
	}
	claims, err := parseWebToken(token)
	if err != nil {
		return core.AccountRecord{}, fmt.Errorf("the web session cookie cannot be used: %w", err)
	}
	if exp := webTokenExpiry(claims); !exp.IsZero() && time.Now().After(exp) {
		return core.AccountRecord{}, fmt.Errorf(
			"that web session cookie expired at %s: sign in again in the Tabbit browser and press 导入凭据",
			exp.UTC().Format(time.RFC3339))
	}

	ep := storedEndpoint{
		ID:      webAccountID(claims, token),
		Kind:    kindWebToken,
		Label:   firstNonEmpty(spec.Field("label"), spec.Label, webCookieHost+" · "+shortUID(claims.Sub)),
		BaseURL: c.webBase(),
		Token:   token,
		WebHost: firstNonEmpty(strings.TrimSpace(spec.Field("web_host")), webCookieHost),
		Enabled: spec.EnabledOr(true),
		AddedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if spec.ID != "" {
		ep.ID = spec.ID
	}

	if err := c.upsertEndpoint(ep); err != nil {
		return core.AccountRecord{}, fmt.Errorf("persist web account: %w", err)
	}
	return c.accountRecord(ep, "added; press Test to check it against web.tabbit.com"), nil
}

// upsertEndpoint stores ep, keeping the original added_at when the row already
// exists so the panel does not claim an edited account was added just now.
func (c *Client) upsertEndpoint(ep storedEndpoint) error {
	_, err := c.mutateEndpoints(func(f *endpointFile) bool {
		if i := f.index(ep.ID); i >= 0 {
			if f.Endpoints[i].AddedAt != "" {
				ep.AddedAt = f.Endpoints[i].AddedAt
			}
			f.Endpoints[i] = ep
			return true
		}
		f.Endpoints = append(f.Endpoints, ep)
		return true
	})
	return err
}

// accountRecord renders the record AddAccount hands back to the panel.
func (c *Client) accountRecord(ep storedEndpoint, note string) core.AccountRecord {
	rec := core.AccountRecord{
		ID:      ep.ID,
		Label:   endpointLabel(ep),
		Enabled: ep.Enabled,
		State:   epStateUnknown,
		Note:    note,
	}
	if epKind(ep) == kindWebToken {
		c.fillWebRecord(&rec, ep)
		return rec
	}
	rec.Fields = map[string]any{
		"origin":      epOriginStored,
		"managed":     true,
		"kind":        kindSidecar,
		"base_url":    ep.BaseURL,
		"has_api_key": ep.APIKey != "",
		"web_host":    ep.WebHost,
		"added_at":    ep.AddedAt,
	}
	return rec
}

// RemoveAccount deletes an endpoint the panel added.  An endpoint that came
// from config or the environment is not the panel's to delete.
func (c *Client) RemoveAccount(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("account id is required")
	}
	changed, err := c.mutateEndpoints(func(f *endpointFile) bool {
		i := f.index(id)
		if i < 0 {
			return false
		}
		f.Endpoints = append(f.Endpoints[:i], f.Endpoints[i+1:]...)
		return true
	})
	if err != nil {
		return fmt.Errorf("persist endpoint removal: %w", err)
	}
	if !changed {
		return fmt.Errorf("endpoint %q was not added through the panel; nothing to remove", id)
	}
	return nil
}

// SetAccountEnabled turns one endpoint on or off.  A disabled endpoint is
// skipped by locate(), which is what makes it usable as a soft delete.
func (c *Client) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("account id is required")
	}
	changed, err := c.mutateEndpoints(func(f *endpointFile) bool {
		i := f.index(id)
		if i < 0 {
			return false
		}
		f.Endpoints[i].Enabled = enabled
		return true
	})
	if err != nil {
		return fmt.Errorf("persist endpoint state: %w", err)
	}
	if !changed {
		return fmt.Errorf("endpoint %q is not managed by the panel", id)
	}
	return nil
}

// TestAccount probes one endpoint and then runs a real completion through it.
//
// A dead sidecar is a RESULT, not a Go error: the panel is asking "is this
// endpoint usable", and "no, nothing is listening" is a valid answer.  Only an
// unknown id is a Go error.
func (c *Client) TestAccount(ctx context.Context, id string) (core.TestResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ep, ok := c.lookupEndpoint(id)
	if !ok {
		return core.TestResult{}, fmt.Errorf("endpoint %q not found", id)
	}
	if epKind(ep) == kindWebToken {
		return c.testWebAccount(ctx, ep), nil
	}

	loc := c.endpointLocation(ep)
	res := core.TestResult{AccountID: ep.ID}
	start := time.Now()

	if st := c.probe(ctx, loc, c.cfg.Timeouts.healthBudget()); !st.ok {
		res.Error = "sidecar did not answer " + c.cfg.HealthPath + ": " + truncate(st.err, 200)
		res.ElapsedMS = time.Since(start).Milliseconds()
		return res, nil
	}

	model := c.testModel()
	if model == "" {
		res.Error = "no model id is known, so the endpoint could not be exercised"
		res.ElapsedMS = time.Since(start).Milliseconds()
		return res, nil
	}
	res.Model = model

	maxTok := 16
	req := &core.ChatRequest{
		Model:     model,
		Messages:  []core.Message{{Role: "user", Content: testPrompt}},
		MaxTokens: &maxTok,
		Stream:    false,
	}
	body, err := buildChatBody(c.cfg, req)
	if err != nil {
		res.Error = err.Error()
		res.ElapsedMS = time.Since(start).Milliseconds()
		return res, nil
	}

	rctx := ctx
	if d := c.cfg.Timeouts.requestBudget(); d > 0 {
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	stream, err := c.openStream(rctx, loc, body)
	if err != nil {
		res.Error = truncate(err.Error(), 300)
		res.ElapsedMS = time.Since(start).Milliseconds()
		return res, nil
	}
	reply, err := drainTabbitStream(stream)
	if err != nil {
		res.Error = truncate(err.Error(), 300)
	} else {
		res.OK = true
		res.Reply = truncate(strings.TrimSpace(reply), 200)
	}
	res.ElapsedMS = time.Since(start).Milliseconds()
	return res, nil
}

// RefreshAccount re-probes.  tabbit has no renewable credential, so "refresh"
// honestly means "check it still answers" rather than inventing a token
// exchange that does not exist.
func (c *Client) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var targets []storedEndpoint
	for _, ep := range c.endpointsSnapshot() {
		if id == "" || ep.ID == id {
			targets = append(targets, ep)
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no endpoint matches %q", id)
	}

	out := make([]core.RefreshResult, 0, len(targets))
	for _, ep := range targets {
		rr := core.RefreshResult{AccountID: ep.ID}
		if !ep.Enabled {
			rr.Error = "endpoint is disabled"
			out = append(out, rr)
			continue
		}
		if epKind(ep) == kindWebToken {
			// A web account has no sidecar to probe: "still good?" means "does
			// the vendor still answer this cookie?".
			wa := c.webAuthFrom(ep.Token, endpointLabel(ep), "panel", ep.BaseURL)
			if _, err := c.verifyWeb(ctx, wa); err != nil {
				rr.Error = truncate(err.Error(), 200)
			} else {
				rr.OK = true
			}
			out = append(out, rr)
			continue
		}
		st := c.probe(ctx, c.endpointLocation(ep), c.cfg.Timeouts.healthBudget())
		if st.ok {
			rr.OK = true
		} else {
			rr.Error = "still unreachable: " + truncate(st.err, 200)
		}
		out = append(out, rr)
	}
	return out, nil
}

// lookupEndpoint resolves an id against the panel store first and then against
// the implicitly resolved endpoint, so Test works on either.
func (c *Client) lookupEndpoint(id string) (storedEndpoint, bool) {
	for _, ep := range c.endpointsSnapshot() {
		if ep.ID == id {
			return ep, true
		}
	}
	loc := c.locate()
	if loc.baseURL != "" && loc.baseURL == id {
		return storedEndpoint{ID: loc.baseURL, BaseURL: loc.baseURL, APIKey: loc.apiKey, Enabled: true}, true
	}
	return storedEndpoint{}, false
}

// testModel picks the cheapest id the module already knows about.
func (c *Client) testModel() string {
	if models, ok := c.cachedCatalog(); ok && len(models) > 0 {
		return stripModelPrefix(models[0].ID, c.cfg.ModelPrefix)
	}
	fallback := fallbackModels(c.cfg)
	if len(fallback) > 0 {
		return stripModelPrefix(fallback[0].ID, c.cfg.ModelPrefix)
	}
	return ""
}

// ---------------------------------------------------------------------------
// CredentialImporter
// ---------------------------------------------------------------------------

// Discover probes every endpoint source this module knows about and reports
// the ones that actually answer.  Only a live sidecar is worth importing.
func (c *Client) Discover(ctx context.Context) ([]core.DiscoveredCredential, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	known := map[string]bool{}
	for _, ep := range c.endpointsSnapshot() {
		known[ep.BaseURL] = true
	}

	type cand struct {
		base   string
		kind   string
		source string
	}
	cands := make([]cand, 0, len(defaultCandidates)+3)
	seen := map[string]bool{}
	push := func(base, kind, source string) {
		base = strings.TrimRight(strings.TrimSpace(base), "/")
		if base == "" || seen[base] {
			return
		}
		seen[base] = true
		cands = append(cands, cand{base: base, kind: kind, source: source})
	}

	if c.cfg.BaseURL != "" {
		push(c.cfg.BaseURL, "config", "config")
	}
	push(c.env("CLIENT2API_TABBIT_BASE_URL"), "env", "env")
	push(c.env("TABBIT_BASE_URL"), "env", "env")
	st := c.readState()
	push(st.BaseURL, "state file", "state")
	for _, d := range defaultCandidates {
		push(d, "default probe", "default")
	}

	budget := c.cfg.Timeouts.healthBudget()
	out := make([]core.DiscoveredCredential, 0, len(cands))
	for _, cd := range cands {
		loc := location{baseURL: cd.base, apiKey: c.cfg.APIKey, source: cd.source}
		if loc.apiKey == "" {
			loc.apiKey = defaultAPIKey
		}
		dc := core.DiscoveredCredential{
			Path:  cd.base,
			Kind:  cd.kind,
			Label: cd.base,
			Note:  "from " + cd.source,
		}
		if hs := c.probe(ctx, loc, budget); hs.ok {
			dc.Importable = true
			dc.Note = "answered " + c.cfg.HealthPath + " (" + sidecarNote(hs) + ")"
		} else {
			dc.Note = "no answer on " + c.cfg.HealthPath + ": " + truncate(hs.err, 160)
		}
		dc.Imported = known[cd.base]
		out = append(out, dc)
	}

	// The Tabbit browser's own session cookie is a credential too, and it is
	// what the panel's import tab is really for.  It cannot be probed with a
	// plain HTTP call from here, so this module reports it as a candidate and
	// lets Import run the browser program that reads it.
	if cookie, ok := c.discoverBrowserCookie(ctx); ok {
		out = append([]core.DiscoveredCredential{cookie}, out...)
	}
	return out, nil
}

// Import adds the given endpoints (or every live one when paths is empty and
// all is true) to the panel store.
func (c *Client) Import(ctx context.Context, paths []string, all bool) ([]core.AccountRecord, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	targets := append([]string(nil), paths...)
	if len(targets) == 0 && all {
		found, err := c.Discover(ctx)
		if err != nil {
			return nil, err
		}
		for _, d := range found {
			if d.Importable && (!d.Imported || d.Kind == browserCookieKind) {
				targets = append(targets, d.Path)
			}
		}
	}
	if len(targets) == 0 {
		return nil, errors.New("nothing to import: no importable endpoint was named")
	}

	// A sweep (paths empty, all true) is best effort: one source being
	// unreachable must not discard the accounts the other sources imported.  A
	// caller that names the browser cookie explicitly still gets the error.
	explicit := len(paths) > 0

	out := make([]core.AccountRecord, 0, len(targets))
	var skipped []string
	for _, t := range targets {
		if t == browserCookiePath {
			rec, err := c.importBrowserCookie(ctx)
			if err != nil {
				if explicit {
					return out, fmt.Errorf("import %s: %w", t, err)
				}
				c.deps.Log("tabbit: skipping the browser cookie during import all: %v", err)
				skipped = append(skipped, err.Error())
				continue
			}
			out = append(out, rec)
			continue
		}
		rec, err := c.AddAccount(ctx, core.AccountSpec{
			Label:  t,
			Fields: map[string]string{"base_url": t},
		})
		if err != nil {
			return out, fmt.Errorf("import %s: %w", t, err)
		}
		out = append(out, rec)
	}
	if len(out) == 0 && len(skipped) > 0 {
		return nil, fmt.Errorf("nothing could be imported: %s", strings.Join(skipped, "; "))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// healthSnapshot copies the last health observation out from under the lock.
func (c *Client) healthSnapshot() (healthState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.health, !c.health.at.IsZero()
}

// drainTabbitStream reads a completion to its end and returns the text.
func drainTabbitStream(s core.Stream) (string, error) {
	text, _, err := drainTabbitChannels(s)
	return text, err
}

// drainTabbitChannels reads a completion to its end and returns the answer text
// and the reasoning, kept apart because a transport can supply one without the
// other.  Merging them would hide a dropped channel behind a plausible string.
func drainTabbitChannels(s core.Stream) (text, reasoning string, err error) {
	defer s.Close()
	var answer, think strings.Builder
	for {
		ev, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return answer.String(), think.String(), nil
		}
		if err != nil {
			return answer.String(), think.String(), err
		}
		switch ev.Type {
		case core.EventDelta:
			answer.WriteString(ev.Delta)
			think.WriteString(ev.Reasoning)
		case core.EventError:
			if ev.Err != nil {
				return answer.String(), think.String(), ev.Err
			}
		case core.EventDone:
			return answer.String(), think.String(), nil
		}
	}
}
