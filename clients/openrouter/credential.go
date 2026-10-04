package openrouter

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

// envAPIKey is the environment variable OpenRouter's own tooling uses.  The
// name is exact: a typo here would silently drop a working credential.
const envAPIKey = "OPENROUTER_API_KEY"

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

var keyPattern = regexp.MustCompile(`sk-or-[A-Za-z0-9._-]{8,}`)

// looksLikeAPIKey is the "do not render this" test used by safeLabel.
func looksLikeAPIKey(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "sk-or-") {
		return true
	}
	if len(s) < 24 || strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == '~':
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
// explicit config first, then the environment, then the imported file.  A key
// that appears in more than one place becomes ONE record at the highest
// precedence, because the same key is the same account however it was supplied.
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
		add(r.APIKey, r.Label, r.Source, false)
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
// a secret into the data directory for no benefit.
func (c *Client) persistCredentials() {
	recs := c.pool.snapshot()
	out := make([]credentialRecord, 0, len(recs))
	for _, r := range recs {
		if r.Source != sourceImported && r.Source != sourcePanel {
			continue
		}
		out = append(out, credentialRecord{
			ID:      r.ID,
			Label:   r.Label,
			APIKey:  r.APIKey,
			Source:  r.Source,
			AddedAt: r.AddedAt,
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
	out := make([]core.DiscoveredCredential, 0, 4)

	store := c.credentialsPath()
	owned := 0
	for _, r := range loadCredentials(store) {
		if r.APIKey != "" {
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
		Label:      "OpenRouter credential store",
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

	// Conventional locations.  OpenRouter ships no desktop client and this
	// machine has no such file, so these are reported as candidates with an
	// honest note rather than as facts.
	for _, p := range conventionalCredentialPaths() {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		out = append(out, core.DiscoveredCredential{
			Path:       p,
			Kind:       "file",
			Label:      "conventional path",
			Note:       "conventional location for a hand-written key file (not created by any OpenRouter tool)",
			Importable: true,
		})
	}
	return out, nil
}

func conventionalCredentialPaths() []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = append(out,
			filepath.Join(home, ".config", "openrouter", "credentials.json"),
			filepath.Join(home, ".openrouter", "credentials.json"),
		)
	}
	if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
		out = append(out, filepath.Join(appData, "openrouter", "credentials.json"))
	}
	return out
}

func (c *Client) hasKey(key string) bool {
	_, ok := c.pool.byID(accountIDFor(key))
	return ok
}

// --- import ---------------------------------------------------------------

// foundKey is one credential read out of a file.
type foundKey struct {
	Key   string
	Label string
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
			failures = append(failures, fmt.Sprintf("%s: no OpenRouter API key found", p))
			continue
		}
		for _, k := range keys {
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
	if len(data) > 1<<20 {
		return nil, errors.New("file is larger than 1 MiB; refusing to scan it")
	}
	return parseCredentialFile(data), nil
}

// parseCredentialFile accepts the shapes an operator plausibly has:
//   - a bare JSON string            "sk-or-v1-…"
//   - an object with a named field  {"api_key": "sk-or-v1-…"}
//   - a nested object               {"data": {...}} / {"credentials": [...]}
//   - a list of objects             {"accounts": [{"api_key": "…"}]}
//   - plain text                    anything containing sk-or-…
func parseCredentialFile(data []byte) []foundKey {
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
		for _, m := range keyPattern.FindAllString(trimmed, -1) {
			push(m, "")
		}
	}
	return out
}

// credentialFieldNames are checked before walking the object, because an
// explicitly named field is stronger evidence than a positional guess.
var credentialFieldNames = []string{
	"api_key", "apikey", "apiKey", "openrouter_api_key", "OPENROUTER_API_KEY",
	"key", "token", "access_token", "secret",
}

// containerFieldNames are walked when no named credential field is present.
var containerFieldNames = []string{"data", "credentials", "accounts", "openrouter", "result"}

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
