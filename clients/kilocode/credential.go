package kilocode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"client2api/internal/core"
)

// envAPIKey is the environment variable the module reads.  The name is exact:
// a typo here would silently drop a working credential.
const envAPIKey = "KILO_API_KEY"

// envCredentialPath is the sentinel Discover/Import use for the environment
// variable, which is a credential source but not a file.
const envCredentialPath = "env:" + envAPIKey

// keyFingerprint is a non-reversible digest of a key, and the basis of every
// record id.  Using a digest rather than a fragment of the key means the id can
// be logged, rendered and used as a file key without carrying any part of the
// secret.
func keyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:])[:16]
}

// accountIDFor is the record id for a key.  The same key always produces the
// same id, which is what makes "the config key and an imported copy of the same
// key" collapse into one account instead of two rows.
func accountIDFor(key string) string { return clientName + ":" + keyFingerprint(key) }

// oauthIDForToken is the stable pool id for a signed-in (OAuth) credential.
// The vendor stamps kiloUserId into the token, so re-importing the same sign-in
// or signing in again updates one row; a token without that claim falls back to
// a fingerprint of the token itself.
func oauthIDForToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	if sub := jwtStringClaim(token, "kiloUserId"); sub != "" {
		return oauthAccountID(sub)
	}
	return oauthAccountID(token)
}

// jwtPattern matches the vendor's API keys, which are JWTs (`eyJ…` with two
// dots).  It is deliberately anchored on the JWT header prefix so a stray
// three-part dotted string elsewhere in a settings tree is not mistaken for a
// credential.
var jwtPattern = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}`)

// looksLikeAPIKey is the "do not render this" test used by safeLabel and by the
// settings walker.  A Kilo key is a JWT; a long opaque token is also accepted
// so an operator's own key shape is still recognised.
func looksLikeAPIKey(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "eyJ") && strings.Count(s, ".") == 2 {
		return true
	}
	if len(s) < 32 || strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == '~' || r == '|':
		default:
			return false
		}
	}
	return true
}

// --- paths ----------------------------------------------------------------

func (c *Client) credentialsPath() string {
	return filepath.Join(c.deps.DataDir, "credentials.json")
}

func (c *Client) statePath() string {
	return filepath.Join(c.deps.DataDir, "state.json")
}

// --- credential assembly --------------------------------------------------

// buildCredentials assembles the pool in the documented precedence order:
// explicit config first, then the environment, then the imported file, and
// finally the gateway's own anonymous free-tier identity.  A key that appears
// in more than one place becomes ONE record at the highest precedence, because
// the same key is the same account however it was supplied.
func (c *Client) buildCredentials() []accountRecord {
	now := c.now()
	state := loadState(c.statePath())

	var recs []accountRecord
	seen := make(map[string]bool)

	add := func(key, label, source string, disabled bool) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		fp := keyFingerprint(key)
		if seen[fp] {
			return
		}
		seen[fp] = true
		recs = append(recs, accountRecord{
			ID:       accountIDFor(key),
			Label:    strings.TrimSpace(label),
			APIKey:   key,
			Source:   source,
			AddedAt:  now.UTC().Format(time.RFC3339),
			Disabled: disabled,
		})
	}

	// addOAuth appends a signed-in account.  Its secret is the access token,
	// and its id derives from the vendor user id so a second sign-in updates
	// the row instead of stacking a new one.
	addOAuth := func(r credentialRecord) {
		token := strings.TrimSpace(firstNonEmpty(r.AccessToken, r.RefreshToken))
		if token == "" {
			return
		}
		fp := keyFingerprint(token)
		if seen[fp] {
			return
		}
		seen[fp] = true
		id := strings.TrimSpace(r.ID)
		if id == "" {
			id = oauthIDForToken(token)
		}
		source := strings.TrimSpace(r.Source)
		if source == "" {
			source = sourceImported
		}
		expires := strings.TrimSpace(r.ExpiresAt)
		if expires == "" {
			if exp := jwtExpiry(token); !exp.IsZero() {
				expires = exp.Format(time.RFC3339)
			}
		}
		added := strings.TrimSpace(r.AddedAt)
		if added == "" {
			added = now.UTC().Format(time.RFC3339)
		}
		recs = append(recs, accountRecord{
			ID:           id,
			Label:        strings.TrimSpace(r.Label),
			AuthMode:     authModeOAuth,
			AccessToken:  strings.TrimSpace(r.AccessToken),
			RefreshToken: strings.TrimSpace(firstNonEmpty(r.RefreshToken, r.AccessToken)),
			ExpiresAt:    expires,
			OAuthServer:  strings.TrimSpace(r.OAuthServer),
			Source:       source,
			AddedAt:      added,
		})
	}

	add(c.cfg.APIKey, "configured key", sourceConfig, false)
	for _, a := range c.cfg.Accounts {
		label := a.Label
		if label == "" {
			label = a.ID
		}
		add(a.APIKey, label, sourceConfig, a.Disabled)
	}
	add(os.Getenv(envAPIKey), envAPIKey, sourceEnv, false)
	for _, r := range loadCredentials(c.credentialsPath()) {
		if r.AccessToken != "" || r.RefreshToken != "" {
			addOAuth(r)
			continue
		}
		add(r.APIKey, r.Label, r.Source, false)
	}
	if c.cfg.anonymousEnabled() {
		recs = append(recs, accountRecord{
			ID:        anonymousID,
			Label:     "anonymous (free)",
			Source:    sourceAnonymous,
			Anonymous: true,
			AddedAt:   now.UTC().Format(time.RFC3339),
		})
	}

	for i := range recs {
		if h, ok := state[recs[i].ID]; ok {
			recs[i] = applyHealth(recs[i], h)
		}
	}
	return recs
}

// persistState writes the health of every record.  It is called after anything
// that changes what the panel shows.
func (c *Client) persistState() {
	recs := c.pool.snapshot()
	out := make([]healthRecord, 0, len(recs))
	for _, r := range recs {
		out = append(out, healthOf(r))
	}
	if err := saveState(c.statePath(), out); err != nil {
		c.noteError("state write failed: " + err.Error())
	}
}

// persistCredentials writes only the records the module owns.  A config or
// environment key is already durable elsewhere; copying it here would duplicate
// a secret into the data directory for no benefit.  The anonymous identity has
// no secret and is rebuilt on every start, so it is not written either.
func (c *Client) persistCredentials() {
	recs := c.pool.snapshot()
	out := make([]credentialRecord, 0, len(recs))
	for _, r := range recs {
		if r.Source != sourceImported && r.Source != sourcePanel {
			continue
		}
		out = append(out, credentialRecord{
			ID:           r.ID,
			Label:        r.Label,
			APIKey:       r.APIKey,
			Source:       r.Source,
			AddedAt:      r.AddedAt,
			AuthMode:     r.AuthMode,
			AccessToken:  r.AccessToken,
			RefreshToken: r.RefreshToken,
			ExpiresAt:    r.ExpiresAt,
			OAuthServer:  r.OAuthServer,
		})
	}
	if err := saveCredentials(c.credentialsPath(), out); err != nil {
		c.noteError("credential write failed: " + err.Error())
	}
}

// persist writes both files.  Convenience for the panel paths, which always
// change something the operator can see.
func (c *Client) persist() {
	c.persistCredentials()
	c.persistState()
}

// --- discovery ------------------------------------------------------------

// Discover reports every credential source it can see, read-only.  Finding
// nothing is a fact, not a failure, so it never returns an error.
func (c *Client) Discover(ctx context.Context) ([]core.DiscoveredCredential, error) {
	c.ensure()
	out := make([]core.DiscoveredCredential, 0, 8)

	store := c.credentialsPath()
	owned := 0
	for _, r := range loadCredentials(store) {
		if r.APIKey != "" || r.AccessToken != "" || r.RefreshToken != "" {
			owned++
		}
	}
	note := "the module's own credential store; drop a JSON file here by hand or import one from the panel"
	if owned == 0 {
		note = "not present (no imported credential yet)"
	}
	out = append(out, core.DiscoveredCredential{
		Path:       store,
		Kind:       "file",
		Label:      "Kilo credential store",
		Note:       note,
		Importable: true,
		Imported:   owned > 0,
	})

	env := strings.TrimSpace(os.Getenv(envAPIKey))
	envNote := "not set"
	if env != "" {
		envNote = "set in this process environment (" + core.MaskSecret(env) + ")"
	}
	out = append(out, core.DiscoveredCredential{
		Path:       envCredentialPath,
		Kind:       "env",
		Label:      envAPIKey,
		Note:       envNote,
		Importable: env != "",
		Imported:   env != "" && c.hasKey(env),
	})

	// The Kilo CLI/Desktop sign-in: the device-flow bearer token lives in a
	// plaintext JSON file (no key to paste), so importing it is what lets this
	// module serve the signed-in account's own catalogue and limits.
	for _, p := range oauthCredentialPaths() {
		token, ok := readOAuthTokenFile(p)
		if !ok {
			continue
		}
		out = append(out, core.DiscoveredCredential{
			Path:       p,
			Kind:       "file",
			Label:      "Kilo sign-in (device OAuth)",
			Note:       "the Kilo CLI/Desktop sign-in token; import it to serve this account",
			Importable: true,
			Imported:   c.hasOAuthToken(token),
		})
	}

	// Locations the Kilo Code desktop/CLI tools are known (or plausibly
	// expected) to keep state in.  The extension's sign-in token lives in VS
	// Code's own storage, which is not a readable file here, so these are
	// reported as candidates with an honest note rather than as facts.
	for _, p := range conventionalCredentialPaths() {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		out = append(out, core.DiscoveredCredential{
			Path:       p,
			Kind:       "file",
			Label:      "Kilo Code state",
			Note:       "a Kilo Code settings/state file; the module walks it for an API key",
			Importable: true,
		})
	}
	return out, nil
}

func conventionalCredentialPaths() []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = append(out,
			filepath.Join(home, ".kilocode", "credentials.json"),
			filepath.Join(home, ".kilocode", "data", "settings", "providers.json"),
			filepath.Join(home, ".kilo", "credentials.json"),
			filepath.Join(home, ".config", "kilocode", "credentials.json"),
		)
	}
	if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
		out = append(out,
			filepath.Join(appData, "kilocode", "credentials.json"),
			filepath.Join(appData, "Code", "User", "globalStorage", "kilocode.kilo-code", "settings", "providers.json"),
		)
	}
	return out
}

// oauthCredentialPaths lists the device-flow sign-in files the Kilo CLI and
// Desktop write.  The path is fixed by the vendor's own client: it stores the
// token under $HOME/.local/share/kilo/auth.json on every platform.
func oauthCredentialPaths() []string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return []string{filepath.Join(home, ".local", "share", "kilo", "auth.json")}
	}
	return nil
}

func (c *Client) hasKey(key string) bool {
	_, ok := c.pool.byID(accountIDFor(key))
	return ok
}

func (c *Client) hasOAuthToken(token string) bool {
	id := oauthIDForToken(token)
	if id == "" {
		return false
	}
	_, ok := c.pool.byID(id)
	return ok
}

// --- import ---------------------------------------------------------------

// foundKey is one credential read out of a file.  A static API key fills Key;
// a device-flow sign-in fills the OAuth fields instead.
type foundKey struct {
	Key      string
	Label    string
	AuthMode string

	AccessToken  string
	RefreshToken string
	ExpiresAt    string
}

// Import reads credentials out of the named files (or, with all=true, out of
// every importable source Discover found) and adds them to the module's store.
//
// It is the only call in this module that writes a credential, and it never
// returns one: the result is panel records with the key stripped.
func (c *Client) Import(ctx context.Context, paths []string, all bool) ([]core.AccountRecord, error) {
	c.ensure()
	if len(paths) == 0 && !all {
		return nil, nil
	}

	targets := paths
	explicit := len(paths) > 0
	if all {
		targets = nil
		discovered, err := c.Discover(ctx)
		if err != nil {
			return nil, err
		}
		for _, d := range discovered {
			if d.Importable && !d.Imported {
				targets = append(targets, d.Path)
			}
		}
	}

	now := c.now()
	var out []core.AccountRecord
	var failures []string
	wrote := false
	for _, p := range targets {
		keys, err := readCredentialSource(p)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		if len(keys) == 0 {
			failures = append(failures, fmt.Sprintf("%s: no Kilo credential found", p))
			continue
		}
		for _, k := range keys {
			if k.AuthMode == authModeOAuth || k.AccessToken != "" || k.RefreshToken != "" {
				token := strings.TrimSpace(firstNonEmpty(k.AccessToken, k.RefreshToken))
				id := oauthIDForToken(token)
				if id == "" {
					continue
				}
				if _, ok := c.pool.byID(id); ok {
					if rec, ok := c.panelRecordFor(id, now); ok {
						out = append(out, rec)
					}
					continue
				}
				expires := strings.TrimSpace(k.ExpiresAt)
				if expires == "" {
					if exp := jwtExpiry(token); !exp.IsZero() {
						expires = exp.Format(time.RFC3339)
					}
				}
				label := firstNonEmpty(strings.TrimSpace(k.Label), "Kilo account")
				c.pool.upsert(accountRecord{
					ID:           id,
					Label:        label,
					AuthMode:     authModeOAuth,
					AccessToken:  strings.TrimSpace(k.AccessToken),
					RefreshToken: strings.TrimSpace(firstNonEmpty(k.RefreshToken, k.AccessToken)),
					ExpiresAt:    expires,
					Source:       sourceImported,
					AddedAt:      now.UTC().Format(time.RFC3339),
				})
				wrote = true
				if rec, ok := c.panelRecordFor(id, now); ok {
					out = append(out, rec)
				}
				continue
			}
			id := accountIDFor(k.Key)
			if _, ok := c.pool.byID(id); ok {
				// Already known through config, environment or an earlier
				// import.  Report it rather than writing a duplicate.
				if rec, ok := c.panelRecordFor(id, now); ok {
					out = append(out, rec)
				}
				continue
			}
			label := k.Label
			if label == "" {
				label = "imported key"
			}
			c.pool.upsert(accountRecord{
				ID:      id,
				Label:   label,
				APIKey:  k.Key,
				Source:  sourceImported,
				AddedAt: now.UTC().Format(time.RFC3339),
			})
			wrote = true
			if rec, ok := c.panelRecordFor(id, now); ok {
				out = append(out, rec)
			}
		}
	}
	if wrote {
		c.persist()
	}
	if len(failures) > 0 {
		c.noteError("import: " + strings.Join(failures, "; "))
	}
	if len(out) == 0 {
		if len(failures) > 0 {
			return nil, errors.New("nothing imported: " + strings.Join(failures, "; "))
		}
		if explicit {
			return nil, errors.New("nothing imported: no credential found in the given path(s)")
		}
	}
	return out, nil
}

func (c *Client) panelRecordFor(id string, now time.Time) (core.AccountRecord, bool) {
	for _, r := range c.pool.records(now) {
		if r.ID == id {
			return r, true
		}
	}
	return core.AccountRecord{}, false
}

// readCredentialSource reads one source.  `env:NAME` is handled as an
// environment lookup; everything else is a file.
func readCredentialSource(path string) ([]foundKey, error) {
	if strings.HasPrefix(path, "env:") {
		name := strings.TrimPrefix(path, "env:")
		val := strings.TrimSpace(os.Getenv(name))
		if val == "" {
			return nil, fmt.Errorf("environment variable %s is not set", name)
		}
		return []foundKey{{Key: val, Label: name}}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > 2<<20 {
		return nil, errors.New("file is larger than 2 MiB; refusing to scan it")
	}
	return parseCredentialFile(data), nil
}

// readOAuthTokenFile reads a device-flow sign-in file and returns its access
// token, or ok=false when the file is absent or does not carry one.
func readOAuthTokenFile(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 2<<20 {
		return "", false
	}
	if fk, ok := parseOAuthAuthJSON(data); ok {
		return strings.TrimSpace(firstNonEmpty(fk.AccessToken, fk.RefreshToken)), true
	}
	return "", false
}

// parseCredentialFile accepts the shapes an operator plausibly has:
//   - a bare JSON string            "eyJ…"
//   - an object with a named field  {"api_key": "eyJ…"}
//   - a nested object               {"data": {...}} / {"credentials": [...]}
//   - a list of objects             {"accounts": [{"api_key": "…"}]}
//   - a device sign-in file         {"kilo": {"type":"oauth","access":"…"}}
//   - plain text                    anything containing a JWT
func parseCredentialFile(data []byte) []foundKey {
	if fk, ok := parseOAuthAuthJSON(data); ok {
		return []foundKey{fk}
	}
	var out []foundKey
	seen := make(map[string]bool)
	push := func(key, label string) {
		key = strings.TrimSpace(key)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, foundKey{Key: key, Label: strings.TrimSpace(label)})
	}

	trimmed := strings.TrimSpace(string(data))
	if trimmed != "" {
		var asString string
		if err := json.Unmarshal([]byte(trimmed), &asString); err == nil {
			push(asString, "")
		} else {
			var asAny any
			if err := json.Unmarshal([]byte(trimmed), &asAny); err == nil {
				collectKeys(asAny, 0, push)
			}
		}
	}
	if len(out) == 0 {
		for _, m := range jwtPattern.FindAllString(trimmed, -1) {
			push(m, "")
		}
	}
	return out
}

// parseOAuthAuthJSON recognises the device-flow sign-in shape Kilo's own tools
// write: either a provider-wrapped object `{"kilo":{...}}` or the bare object.
// Only an `"type":"oauth"` object with a JWT-shaped access token qualifies, so
// an ordinary settings file is never mistaken for a sign-in.
func parseOAuthAuthJSON(data []byte) (foundKey, bool) {
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return foundKey{}, false
	}
	if fk, ok := oauthFromObject(root); ok {
		return fk, true
	}
	for _, v := range root {
		obj, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if fk, ok := oauthFromObject(obj); ok {
			return fk, true
		}
	}
	return foundKey{}, false
}

func oauthFromObject(obj map[string]any) (foundKey, bool) {
	kind, _ := obj["type"].(string)
	if !strings.EqualFold(strings.TrimSpace(kind), "oauth") {
		return foundKey{}, false
	}
	access := firstJSONString(obj, "access", "access_token", "token")
	if !looksLikeAPIKey(access) {
		return foundKey{}, false
	}
	refresh := firstJSONString(obj, "refresh", "refresh_token")
	expires := parseExpiryValue(obj["expires"])
	if expires == "" {
		expires = parseExpiryValue(obj["expires_at"])
	}
	return foundKey{
		AuthMode:     authModeOAuth,
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresAt:    expires,
	}, true
}

// firstJSONString returns the first non-empty string under any of keys.
func firstJSONString(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := obj[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// parseExpiryValue turns a sign-in file's `expires` into an RFC3339 string.
// The vendor writes epoch MILLISECONDS, but a number below 1e12 is read as
// seconds so a seconds-based mirror is also accepted.  Anything unrecognised
// answers "", which callers treat as "no expiry".
func parseExpiryValue(v any) string {
	switch t := v.(type) {
	case float64:
		return formatEpoch(int64(t))
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return formatEpoch(n)
		}
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return ""
		}
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			return ts.UTC().Format(time.RFC3339)
		}
		var n json.Number
		if err := json.Unmarshal([]byte(s), &n); err == nil {
			return parseExpiryValue(n)
		}
	}
	return ""
}

func formatEpoch(n int64) string {
	if n <= 0 {
		return ""
	}
	if n < 1e12 {
		return time.Unix(n, 0).UTC().Format(time.RFC3339)
	}
	return time.UnixMilli(n).UTC().Format(time.RFC3339)
}

// credentialFieldNames are checked before walking the object, because an
// explicitly named field is stronger evidence than a positional guess.
var credentialFieldNames = []string{
	"api_key", "apikey", "apiKey", "kilo_api_key", "KILO_API_KEY",
	"kilocode_api_key", "kilocodeToken", "kiloToken",
	"key", "token", "access_token", "secret",
}

// containerFieldNames are walked when no named credential field is present.
var containerFieldNames = []string{"data", "credentials", "accounts", "kilo", "kilocode", "result", "providers"}

func collectKeys(v any, depth int, push func(key, label string)) {
	if depth > 6 {
		return
	}
	switch t := v.(type) {
	case string:
		if looksLikeAPIKey(t) {
			push(t, "")
		}
	case []any:
		for _, item := range t {
			collectKeys(item, depth+1, push)
		}
	case map[string]any:
		label, _ := t["label"].(string)
		if label == "" {
			label, _ = t["name"].(string)
		}
		for _, name := range credentialFieldNames {
			for k, raw := range t {
				if !strings.EqualFold(k, name) {
					continue
				}
				if s, ok := raw.(string); ok && looksLikeAPIKey(s) {
					push(s, label)
				}
			}
		}
		for _, name := range containerFieldNames {
			for k, raw := range t {
				if !strings.EqualFold(k, name) {
					continue
				}
				switch raw.(type) {
				case map[string]any, []any:
					collectKeys(raw, depth+1, push)
				}
			}
		}
	}
}
