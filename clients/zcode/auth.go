package zcode

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"client2api/internal/core"
)

// Credential channels.
const (
	modeAPIKey = "api_key"
	modeJWT    = "jwt"

	providerZai      = "zai"
	providerBigmodel = "bigmodel"

	// encryptedValuePrefix marks values the desktop client stored with its own
	// envelope encryption.  We can neither read nor use them.
	encryptedValuePrefix = "enc:v1:"
)

// Account lifecycle states, matching core.AccountStatus.State.
const (
	stateReady     = "ready"
	stateCooling   = "cooling"
	stateExhausted = "exhausted"
	stateInvalid   = "invalid"
	stateUnknown   = "unknown"
)

// Account is one usable credential plus its runtime state.  Secrets live in
// memory only; the persisted state file never contains them.
type Account struct {
	ID       string `json:"id"`
	Label    string `json:"label,omitempty"`
	Provider string `json:"provider"`
	Mode     string `json:"mode"`
	BaseURL  string `json:"base_url,omitempty"`
	Enabled  bool   `json:"enabled"`
	Source   string `json:"source,omitempty"`
	// Origin records how the credential got here: "panel-login" for a
	// credential the panel sign-in flow created, otherwise the source string.
	Origin string `json:"origin,omitempty"`
	UserID string `json:"user_id,omitempty"`

	State         string    `json:"state,omitempty"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
	ExpiresAt     time.Time `json:"expires_at,omitempty"`
	Note          string    `json:"note,omitempty"`
	LastError     string    `json:"last_error,omitempty"`

	apiKey string
	jwt    string
}

// secret returns the credential actually sent upstream.
func (a *Account) secret() string {
	if a.Mode == modeJWT {
		return a.jwt
	}
	return a.apiKey
}

// ---------------------------------------------------------------------------
// On-disk state
// ---------------------------------------------------------------------------

// persistedAccount deliberately has no secret fields.
type persistedAccount struct {
	ID            string    `json:"id"`
	Label         string    `json:"label,omitempty"`
	Enabled       *bool     `json:"enabled,omitempty"`
	State         string    `json:"state,omitempty"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
	ExpiresAt     time.Time `json:"expires_at,omitempty"`
	Note          string    `json:"note,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
}

type persistedState struct {
	Version  int                `json:"version"`
	Accounts []persistedAccount `json:"accounts"`
	// Removed is the tombstone set: account IDs the operator deleted through
	// the panel.  It is needed because a credential that discovery re-reads
	// from the desktop client would otherwise come back on every restart.
	Removed []string `json:"removed,omitempty"`
}

const stateVersion = 1

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

// credentialSource is one credential found on the machine, before state is
// merged in, together with the store it was read from.
//
// The embedded Account holds the secret.  It is deliberately package-private:
// a credentialSource may be turned into an account, but only the redacted
// projection (see accounts.go) is ever handed to the panel.
type credentialSource struct {
	Account

	// Path identifies this exact credential.  A store that holds several
	// credentials uses "<file>#<fragment>"; a bare file path addresses every
	// credential in that file.
	Path string

	// Kind is the credential kind the panel shows: "api-key", "jwt", or
	// "electron-profile" for a place that was looked at but cannot be read.
	Kind string

	// Note explains what this credential is.
	Note string

	// Importable is false for a location we inspected but cannot extract a
	// usable credential from.  Such entries are reported so the operator can
	// see that the location was checked, but they can never be imported.
	Importable bool
}

// discover reads the ZCode desktop client's own credential stores.  Anything
// unreadable is skipped silently; discovery must never break the module.
//
// credentials.json is read first: it holds the complete secrets, and config.json
// holds a copy of the same keys truncated to the id half.  Passing the secrets
// forward lets the config.json entries be completed instead of being reported as
// credentials that cannot work.
func discover(logf func(string, ...any)) []credentialSource {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	root := filepath.Join(home, ".zcode", "v2")
	credPath := filepath.Join(root, "credentials.json")
	secrets := credentialSecrets(credPath, logf)
	out := discoverFromProviderConfig(filepath.Join(root, "config.json"), secrets, logf)
	out = append(out, discoverFromCredentials(credPath, logf)...)
	return out
}

// providerSecrets is what the desktop client's credential store knows about the
// API keys it holds, indexed by the id half of each "id.secret" value.
//
// byID lets a config.json entry that the client wrote truncated be completed
// instead of being published as a credential that cannot work.  identity records
// which vendor account each of those keys belongs to, so the panel can show one
// row per account instead of one per credential (see core.AccountRecord.Identity).
//
// The account id is read out of the store's own key name —
// "account-provider:…:account:<userID>:api-key" — which is the vendor's account
// id written down by the vendor's own client.  It is never derived or guessed.
type providerSecrets struct {
	byID     map[string]string // id half -> complete "id.secret"
	identity map[string]string // id half -> vendor account id
}

// credentialSecrets indexes every "id.secret" credential in the desktop
// client's store by its id half, so a provider entry that only carries the id
// can be completed.  Values under a different envelope passphrase are simply
// absent from the index.
func credentialSecrets(path string, logf func(string, ...any)) providerSecrets {
	doc := readJSONMap(path, logf)
	if len(doc) == 0 {
		return providerSecrets{}
	}
	out := providerSecrets{byID: make(map[string]string)}
	for _, name := range sortedKeys(doc) {
		// Only the client's own API-key entries hold "id.secret" pairs; every
		// other value in this store is something else entirely.
		if !isConnectionAPIKeyEntry(name) {
			continue
		}
		raw, ok := doc[name].(string)
		if !ok {
			continue
		}
		secret, ok := openCredential(raw)
		if !ok {
			continue
		}
		// A JWT is three dot-separated parts and is never the secret half of a
		// provider key, so it must not enter the index (its first part would
		// otherwise masquerade as a key id).
		if looksLikeJWT(secret) {
			continue
		}
		id, _, found := strings.Cut(secret, ".")
		if !found || strings.TrimSpace(id) == "" {
			continue
		}
		id = strings.TrimSpace(id)
		mergeProviderSecret(&out, id, secret, userIDForName(name))
		// The key name carries the account the credential was minted for.  A
		// name that does not spell one out leaves the account unknown, which is
		// the honest answer, so it is simply not recorded.

	}
	return out
}

// userIDForName extracts the vendor account id from a credential-store key
// when the key spells one out, and returns "" otherwise.
func userIDForName(name string) string {
	if _, _, userID, ok := splitProviderAccountKey(name); ok {
		return strings.TrimSpace(userID)
	}
	return ""
}

// mergeProviderSecret records one credential-store secret, preferring the
// complete id.secret form over the truncated id-only copy the desktop client
// writes into config.json.  Both carry the same id half, so first-wins would
// discard the only usable credential.
func mergeProviderSecret(out *providerSecrets, id, secret, userID string) {
	id = strings.TrimSpace(id)
	secret = strings.TrimSpace(secret)
	if out == nil || id == "" || secret == "" {
		return
	}
	if out.byID == nil {
		out.byID = make(map[string]string)
	}
	if prev := out.byID[id]; strings.Contains(prev, ".") && !strings.Contains(secret, ".") {
		return
	}
	out.byID[id] = secret
	if strings.TrimSpace(userID) != "" {
		if out.identity == nil {
			out.identity = make(map[string]string)
		}
		out.identity[id] = strings.TrimSpace(userID)
	}
}

// readJSONMap reads a JSON object, reporting a parse failure instead of
// returning an error: discovery degrades, it does not fail.
func readJSONMap(path string, logf func(string, ...any)) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		if logf != nil {
			logf("zcode: cannot parse %s (%v); skipping", path, err)
		}
		return nil
	}
	return doc
}

// sortedKeys gives a map's keys in a deterministic order, so the account list
// the panel shows does not reshuffle between restarts.
func sortedKeys(m map[string]any) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

type providerEntry struct {
	Enabled bool   `json:"enabled"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Options struct {
		APIKey  string `json:"apiKey"`
		BaseURL string `json:"baseURL"`
	} `json:"options"`
}

// discoverFromProviderConfig reads the desktop client's live provider config.
// secrets carries the complete "id.secret" credentials, and is used to complete
// entries the client wrote truncated — and to say which account an API-key entry
// belongs to, when the store knows.
func discoverFromProviderConfig(path string, secrets providerSecrets, logf func(string, ...any)) []credentialSource {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		Provider map[string]providerEntry `json:"provider"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		if logf != nil {
			logf("zcode: cannot parse %s (%v); skipping", path, err)
		}
		return nil
	}
	names := make([]string, 0, len(doc.Provider))
	for name := range doc.Provider {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic account order

	var out []credentialSource
	for _, name := range names {
		entry := doc.Provider[name]
		secret, ok := openCredential(entry.Options.APIKey)
		if !ok {
			// Empty, or an envelope written under a passphrase this machine
			// cannot reproduce: unreadable, and never guessed at.
			continue
		}
		if entry.Kind != "" && !strings.EqualFold(entry.Kind, "anthropic") {
			continue
		}
		// The client stores the key id alone here and keeps the secret half in
		// credentials.json.  A key without its secret cannot authenticate, so
		// complete it from the store rather than publish a credential that is
		// guaranteed to fail.
		//
		// keyID is the id half either way, which is what the store indexes both
		// the secret and the account id by.
		keyID := strings.TrimSpace(secret)
		if id, _, found := strings.Cut(keyID, "."); found {
			keyID = strings.TrimSpace(id)
		}
		note := "provider " + name + " in the ZCode desktop client config"
		if !strings.Contains(secret, ".") {
			full := secrets.byID[secret]
			if full == "" {
				note += "; the secret half was not readable in the credential store"
			} else {
				secret = full
				note += "; secret half completed from the credential store"
			}
		}
		mode := modeAPIKey
		if looksLikeJWT(secret) {
			mode = modeJWT
		}
		base := strings.TrimSpace(entry.Options.BaseURL)
		provider := providerForHost(base)
		label := strings.TrimSpace(entry.Name)
		if label == "" {
			label = name
		}
		acct := Account{
			ID:       "zcode-config:" + name,
			Label:    label,
			Provider: provider,
			Mode:     mode,
			BaseURL:  base,
			Enabled:  true,
			Source:   "~/.zcode/v2/config.json#provider." + name,
		}
		if mode == modeJWT {
			acct.jwt = secret
			acct.UserID = jwtUserID(secret)
		} else {
			acct.apiKey = secret
			// An API key says nothing about who it was issued to.  The store's
			// key name does, so take the account from there — and when it does
			// not spell one out, leave it unknown rather than guess.
			if userID := secrets.identity[keyID]; userID != "" {
				acct.UserID = userID
			}
		}
		out = append(out, credentialSource{
			Account:    acct,
			Path:       path + "#provider." + name,
			Kind:       kindNameForMode(mode),
			Note:       note,
			Importable: true,
		})
	}
	return out
}

func discoverFromCredentials(path string, logf func(string, ...any)) []credentialSource {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		if logf != nil {
			logf("zcode: cannot parse %s (%v); skipping", path, err)
		}
		return nil
	}
	var out []credentialSource
	if v, ok := doc["zcodejwttoken"].(string); ok {
		token := strings.TrimSpace(v)
		if token != "" && !strings.HasPrefix(token, encryptedValuePrefix) {
			out = append(out, credentialSource{
				Account: Account{
					ID:       "zcode-credentials:zcodejwttoken",
					Label:    "ZCode plan JWT",
					Provider: providerZai,
					Mode:     modeJWT,
					BaseURL:  baseZaiPlan,
					Enabled:  true,
					Source:   "~/.zcode/v2/credentials.json#zcodejwttoken",
					UserID:   jwtUserID(token),
					jwt:      token,
				},
				Path:       path + "#zcodejwttoken",
				Kind:       kindNameForMode(modeJWT),
				Note:       "plan JWT in the ZCode desktop client credential store",
				Importable: true,
			})
		}
	}
	// `oauth:*:access_token` values are OAuth access tokens, not API keys: they
	// cannot authenticate /anthropic.  They are intentionally not turned into
	// accounts (see README § Credential discovery).
	//
	// `account-provider:*:api-key` values are different: they hold the complete
	// "id.secret" API key for a provider connection, encrypted.  That key does
	// authenticate the provider's own /anthropic base, so it becomes an account.
	for _, name := range sortedKeys(doc) {
		acct, ok := providerAccountFromKey(name, doc[name])
		if !ok {
			continue
		}
		out = append(out, credentialSource{
			Account:    acct,
			Path:       path + "#" + name,
			Kind:       kindNameForMode(acct.Mode),
			Note:       "API key for the " + acct.Label + " connection in the ZCode desktop client credential store",
			Importable: true,
		})
	}
	return out
}

// providerAccountPrefix marks the desktop client's per-connection credential
// entries.  A full key looks like
//
//	account-provider:coding-plan:account:bigmodel-individual-coding-plan:account:61161790588087632:api-key
//
// so the provider family and the connection kind are read out of the
// connection segment, and the account identity out of the segment after it.
const providerAccountPrefix = "account-provider:"

// isConnectionAPIKeyEntry reports whether a credential-store key names a
// provider connection's API key.
func isConnectionAPIKeyEntry(name string) bool {
	return strings.HasPrefix(name, providerAccountPrefix) && strings.HasSuffix(name, ":api-key")
}

// providerAccountFromKey turns one credential-store entry into an account.  It
// returns false for anything that is not an API-key entry, is unreadable, or
// does not name a provider family this module knows how to call.
func providerAccountFromKey(name string, value any) (Account, bool) {
	if !isConnectionAPIKeyEntry(name) {
		return Account{}, false
	}
	raw, ok := value.(string)
	if !ok {
		return Account{}, false
	}
	secret, ok := openCredential(raw)
	if !ok {
		return Account{}, false
	}
	provider, kind, userID, ok := splitProviderAccountKey(name)
	if !ok {
		return Account{}, false
	}

	acct := Account{
		ID:       providerAccountID(provider, kind, userID),
		Label:    providerAccountLabel(provider, kind),
		Provider: provider,
		Mode:     modeAPIKey,
		BaseURL:  baseForProvider(provider),
		Enabled:  true,
		Source:   "~/.zcode/v2/credentials.json#" + name,
		UserID:   userID,
	}
	if looksLikeJWT(secret) {
		// A plan JWT found in a connection entry is the same credential the
		// zcodejwttoken path discovers, so it is described the same way.
		acct.Mode = modeJWT
		acct.BaseURL = baseZaiPlan
		acct.jwt = secret
		acct.UserID = firstNonEmpty(jwtUserID(secret), userID)
	} else {
		acct.apiKey = secret
	}
	return acct, true
}

// splitProviderAccountKey pulls the provider family, connection kind and
// account identity out of a credential-store key.  The connection segment is
// recognised by its provider prefix, so an added segment does not break the
// parse the way a fixed index would.
func splitProviderAccountKey(name string) (provider, kind, userID string, ok bool) {
	parts := strings.Split(name, ":")
	for i, part := range parts {
		switch {
		case part == providerBigmodel || strings.HasPrefix(part, providerBigmodel+"-"):
			provider = providerBigmodel
		case part == providerZai || strings.HasPrefix(part, providerZai+"-"):
			provider = providerZai
		default:
			continue
		}
		kind = strings.TrimPrefix(strings.TrimPrefix(part, provider), "-")
		if i+2 < len(parts) && parts[i+1] == "account" {
			userID = parts[i+2]
		}
		return provider, kind, userID, true
	}
	return "", "", "", false
}

// providerAccountID names the discovered account after its connection and
// identity, so the same connection re-read on the next start maps to the same
// row instead of accumulating duplicates.
func providerAccountID(provider, kind, userID string) string {
	id := "zcode-credentials:" + provider
	if kind != "" {
		id += "-" + kind
	}
	if userID != "" {
		id += ":" + userID
	}
	return id
}

// providerAccountLabel renders a human label for a connection kind.
func providerAccountLabel(provider, kind string) string {
	product := "BigModel"
	if !strings.EqualFold(provider, providerBigmodel) {
		product = "Z.ai"
	}
	switch {
	case strings.Contains(kind, "coding-plan"):
		return product + " Coding Plan"
	case kind == "":
		return product + " API Key"
	default:
		return product + " " + kind
	}
}

// baseForProvider picks the API-key endpoint family for a provider.
func baseForProvider(provider string) string {
	if strings.EqualFold(provider, providerBigmodel) {
		return baseBigmodelKey
	}
	return baseZaiAPIKey
}

// firstNonEmpty returns the first non-empty value, so a caller can express a
// preference chain without a temporary variable.  It is defined in pool.go.

// looksLikeJWT reports whether a secret has the three-dot-separated shape.
func looksLikeJWT(secret string) bool {
	if strings.Count(secret, ".") != 2 {
		return false
	}
	head := strings.SplitN(secret, ".", 2)[0]
	decoded, err := base64.RawURLEncoding.DecodeString(head)
	if err != nil {
		return false
	}
	return strings.HasPrefix(string(decoded), "{")
}

// jwtUserID extracts user_id (or sub) from an unsigned JWT payload.  The JWT is
// opaque to us; we only read the identity it carries, so metadata.user_id can be
// injected and the panel can tell which rows are the same account.  No signature
// check is possible or needed.
func jwtUserID(token string) string {
	return core.JWTClaim(token, "user_id", "sub")
}

func providerForHost(base string) string {
	if strings.Contains(strings.ToLower(base), "bigmodel") {
		return providerBigmodel
	}
	return providerZai
}
