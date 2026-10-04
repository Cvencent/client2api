package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Credential discovery.
//
// Zen has exactly one credential shape: a static API key.  There is no login
// flow, no refresh flow, no check-in and no task API, so this module implements
// core.CredentialImporter and nothing else from that family.
//
// The key can arrive from four places, in this order:
//
//  1. the module config (clients.opencode.api_key, or an accounts[] entry)
//  2. the OPENCODE_API_KEY environment variable
//  3. a credential this module previously stored under its own DataDir
//  4. opencode's own auth.json, which is what `opencode auth login` writes
//
// Order only decides which label wins when the same key appears twice — the
// account id is a fingerprint of the key, so two sources holding the same key
// are the same account, not two.
// ---------------------------------------------------------------------------

// apiKeyEnv is the environment variable opencode itself documents for Zen.
const apiKeyEnv = "OPENCODE_API_KEY"

// vendorProviderID is the provider key opencode uses for Zen in auth.json.
// It is `opencode`, not `opencode-zen`: the docs' own config example is
// `"opencode/<model-id>"`.
const vendorProviderID = "opencode"

// opencodeDataDir is where opencode keeps its state.
//
// XDG_DATA_HOME is honoured when set, because that is what opencode itself
// does; otherwise it is ~/.local/share/opencode on every platform, including
// Windows, because opencode is a Node program that follows the XDG convention
// rather than %APPDATA%.
func opencodeDataDir() string {
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" {
		return filepath.Join(xdg, "opencode")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "share", "opencode")
}

// vendorAuthPath is opencode's credential store, or "" when it cannot be
// located.
func vendorAuthPath() string {
	dir := opencodeDataDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "auth.json")
}

// vendorAuthEntry is one provider entry in auth.json.
//
// Shape (verified against the opencode source and its documented format):
//
//	{ "<providerID>": {"type":"api","key":"<key>","metadata":{...}} }
//	{ "<providerID>": {"type":"oauth","refresh":...,"access":...,"expires":...} }
type vendorAuthEntry struct {
	Type     string         `json:"type"`
	Key      string         `json:"key"`
	Access   string         `json:"access"`
	Refresh  string         `json:"refresh"`
	Expires  any            `json:"expires"`
	Metadata map[string]any `json:"metadata"`
}

// vendorAuthResult describes what one auth.json yielded.
type vendorAuthResult struct {
	recs []accountRecord
	// present is true when the file exists and parsed as a provider map.
	present bool
	// note explains the outcome for the panel, in one line.
	note string
}

// readVendorAuthFile reads opencode's auth.json.
//
// Only a `type == "api"` entry for the `opencode` provider carries a key this
// module can use.  An oauth entry is reported but not imported: Zen has no
// refresh flow, so an expired access token could never be renewed here, and
// pretending otherwise would create an account that fails forever.
func readVendorAuthFile(path string) vendorAuthResult {
	if path == "" {
		return vendorAuthResult{}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return vendorAuthResult{}
		}
		return vendorAuthResult{present: true, note: "unreadable: " + core.Redact(err.Error())}
	}

	var doc map[string]vendorAuthEntry
	if err := json.Unmarshal(raw, &doc); err != nil {
		return vendorAuthResult{present: true, note: "not a JSON provider map"}
	}
	if len(doc) == 0 {
		return vendorAuthResult{present: true, note: "no providers"}
	}

	entry, ok := doc[vendorProviderID]
	if !ok {
		for k, v := range doc {
			if strings.EqualFold(k, vendorProviderID) {
				entry, ok = v, true
				break
			}
		}
	}
	if !ok {
		return vendorAuthResult{
			present: true,
			note:    fmt.Sprintf("no %q provider (found: %s)", vendorProviderID, strings.Join(sortedKeys(doc), ", ")),
		}
	}

	switch strings.ToLower(strings.TrimSpace(entry.Type)) {
	case "api":
		key := strings.TrimSpace(entry.Key)
		if key == "" {
			return vendorAuthResult{present: true, note: `provider "opencode" is type api but carries no key`}
		}
		return vendorAuthResult{
			present: true,
			note:    "provider \"opencode\", type api, key " + core.MaskSecret(key),
			recs: []accountRecord{{
				ID:      accountID(keyFingerprint(key)),
				Label:   "opencode auth.json",
				APIKey:  key,
				Enabled: true,
				Source:  sourceImport,
			}},
		}
	case "oauth":
		return vendorAuthResult{
			present: true,
			note:    `provider "opencode" is type oauth; Zen has no refresh flow, so it cannot be imported`,
		}
	default:
		return vendorAuthResult{
			present: true,
			note: fmt.Sprintf("provider %q has unsupported type %q",
				vendorProviderID, firstNonEmpty(entry.Type, "(none)")),
		}
	}
}

// candidate is one credential the module could use, before the pool is built.
type candidate struct {
	key    string
	label  string
	source string
}

// candidates collects every credential source in precedence order.
func (c *Client) candidates() []candidate {
	var out []candidate
	seen := map[string]bool{}
	add := func(key, label, source string) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		fp := keyFingerprint(key)
		if seen[fp] {
			return
		}
		seen[fp] = true
		out = append(out, candidate{key: key, label: label, source: source})
	}

	add(c.cfg.APIKey, "config", sourceConfig)
	for i, a := range c.cfg.Accounts {
		label := a.Label
		if label == "" {
			label = fmt.Sprintf("config account %d", i+1)
		}
		add(firstNonEmpty(a.APIKey, a.Key), label, sourceConfig)
	}
	add(os.Getenv(apiKeyEnv), apiKeyEnv, sourceEnv)
	return out
}

// vendorCandidates reads the vendor auth.json for extra keys.
func (c *Client) vendorCandidates() []candidate {
	res := readVendorAuthFile(c.vendorAuthPathFor())
	out := make([]candidate, 0, len(res.recs))
	for _, r := range res.recs {
		out = append(out, candidate{key: r.APIKey, label: r.Label, source: r.Source})
	}
	return out
}

// vendorAuthPathFor returns the auth.json path unless discovery was disabled.
func (c *Client) vendorAuthPathFor() string {
	if c.cfg.DisableAuthJSONDiscovery {
		return ""
	}
	return vendorAuthPath()
}

// ---------------------------------------------------------------------------
// core.CredentialImporter.
// ---------------------------------------------------------------------------

// Discover reports the credential files this module can see.  It is read-only:
// nothing is written and no account is created.
func (c *Client) Discover(ctx context.Context) ([]core.DiscoveredCredential, error) {
	c.ensure()
	var out []core.DiscoveredCredential

	if p := c.vendorAuthPathFor(); p != "" {
		if res := readVendorAuthFile(p); res.present {
			out = append(out, core.DiscoveredCredential{
				Path:       p,
				Kind:       "opencode-auth",
				Label:      "opencode auth.json",
				Note:       res.note,
				Importable: len(res.recs) > 0,
				Imported:   c.poolHasAll(res.recs),
			})
		}
	}

	for _, dir := range c.discoverDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			// An unreadable or absent directory is not a failure: it just has
			// nothing to offer.
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".json") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			recs, err := c.recordsFromFile(path)
			note := fmt.Sprintf("%d credential(s)", len(recs))
			importable := len(recs) > 0
			if err != nil {
				note = "unreadable: " + core.Redact(err.Error())
				importable = false
			}
			out = append(out, core.DiscoveredCredential{
				Path:       path,
				Kind:       "opencode-json",
				Label:      e.Name(),
				Note:       note,
				Importable: importable,
				Imported:   c.poolHasAll(recs),
			})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// discoverDirs are the module-owned directories scanned for credential files.
//
// The module's own accounts.json is deliberately NOT among them: it is the
// store, not a source, and offering to import it would be a no-op dressed up as
// an action.
func (c *Client) discoverDirs() []string {
	if c.deps.DataDir == "" {
		return nil
	}
	return []string{filepath.Join(c.deps.DataDir, importDirName)}
}

// poolHasAll reports whether every credential in recs is already known.
func (c *Client) poolHasAll(recs []accountRecord) bool {
	if len(recs) == 0 {
		return false
	}
	for _, r := range recs {
		if _, ok := c.pool.byID(r.ID); !ok {
			return false
		}
	}
	return true
}

// Import writes discovered credentials into the pool.  It is the only call in
// this module that writes a credential.
func (c *Client) Import(ctx context.Context, paths []string, all bool) ([]core.AccountRecord, error) {
	c.ensure()
	if len(paths) == 0 && !all {
		return nil, nil
	}
	explicit := len(paths) > 0
	if all {
		found, err := c.Discover(ctx)
		if err != nil {
			return nil, err
		}
		paths = paths[:0]
		for _, d := range found {
			if d.Importable {
				paths = append(paths, d.Path)
			}
		}
	}

	var firstErr error
	imported := 0
	for _, path := range paths {
		recs, err := c.recordsFromFile(path)
		if err != nil {
			// A path the operator named gets its real error; a sweep skips the
			// broken file so one bad leftover cannot hide the good ones.
			if explicit && firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, r := range recs {
			if c.upsertAccount(r) {
				imported++
			}
		}
	}
	c.persist()
	return c.accountRecords(c.now()), firstErr
}

// recordsFromFile reads credentials from one file, routing by shape.
func (c *Client) recordsFromFile(path string) ([]accountRecord, error) {
	if path == "" {
		return nil, fmt.Errorf("%s: empty credential path", clientName)
	}
	if p := c.vendorAuthPathFor(); p != "" && samePath(path, p) {
		res := readVendorAuthFile(path)
		if len(res.recs) == 0 {
			if res.note != "" {
				return nil, fmt.Errorf("%s: %s: %s", clientName, path, res.note)
			}
			return nil, fmt.Errorf("%s: %s carries no usable credential", clientName, path)
		}
		return res.recs, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: reading %s: %w", clientName, path, err)
	}
	recs, err := decodeCredentialsBody(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: parsing %s: %w", clientName, path, err)
	}
	out := make([]accountRecord, 0, len(recs))
	for _, r := range recs {
		if strings.TrimSpace(r.APIKey) == "" {
			continue
		}
		if r.ID == "" {
			r.ID = accountID(keyFingerprint(r.APIKey))
		}
		if r.Source == "" {
			r.Source = sourceImport
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: %s carries no usable credential", clientName, path)
	}
	return out, nil
}

// samePath compares two paths tolerantly; both are produced by this module, so
// a plain cleaned comparison is enough.
func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}
