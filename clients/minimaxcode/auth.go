package minimaxcode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"client2api/internal/core"
)

// accessTokenPrefix is the marker every MiniMax Code access token carries.  It
// is used only to recognise a credential, never to validate one -- the vendor
// decides validity, not a prefix check.
const accessTokenPrefix = "mmoat_"

// refreshTokenPrefix is the matching marker for refresh tokens.
const refreshTokenPrefix = "mmort_"

// authRecord is one entry of the desktop client's credential store.  The field
// names are the client's, camelCase and all.
type authRecord struct {
	SchemaVersion int      `json:"schemaVersion"`
	AccessToken   string   `json:"accessToken"`
	RefreshToken  string   `json:"refreshToken"`
	TokenType     string   `json:"tokenType"`
	ClientID      string   `json:"clientId"`
	Scopes        []string `json:"scopes"`
	Audience      string   `json:"audience"`
	ExpiresAtMs   int64    `json:"expiresAtMs"`
	Generation    int      `json:"generation"`
	LoginEpoch    string   `json:"loginEpoch"`
}

type authFile struct {
	SchemaVersion int                   `json:"schemaVersion"`
	Records       map[string]authRecord `json:"records"`
}

// authState mirrors the sibling auth-state.json.  It is only ever used as
// evidence that the desktop client considers itself signed in; the credential
// itself always comes from auth.json.
type authState struct {
	SchemaVersion int    `json:"schemaVersion"`
	Status        string `json:"status"`
	StoreKind     string `json:"storeKind"`
	ClientID      string `json:"clientId"`
	BuildEnv      string `json:"buildEnv"`
	Region        string `json:"region"`
	Generation    int    `json:"generation"`
	ExpiresAtMs   int64  `json:"expiresAtMs"`
}

// credential is one usable token discovered on disk, together with everything
// needed to write a refreshed token back where the desktop client will find it.
type credential struct {
	// ID is stable across re-logins: it is derived from where the credential
	// lives, not from the token, so the panel's row survives a refresh.
	ID string
	// Path is the auth.json this credential came from.
	Path string
	// RecordKey is the key inside that file, which contains a literal NUL and
	// must be echoed back verbatim when writing.
	RecordKey string

	BuildEnv string
	Region   string
	ClientID string

	Access     string
	Refresh    string
	ExpiresAt  time.Time
	Generation int

	// Label is a human name for the account, from the desktop client's own
	// shared-user record when it can be read.
	Label string
}

// expired reports whether the credential needs refreshing, allowing a little
// leeway because a token that expires in flight is indistinguishable from a
// rejected one.
func (c credential) expired(now time.Time) bool {
	if c.ExpiresAt.IsZero() {
		return false // no expiry recorded: let the vendor be the judge
	}
	return !now.Add(refreshLeeway).Before(c.ExpiresAt)
}

// accountID builds the stable identifier for a credential location.
func accountID(buildEnv, region, clientID string) string {
	parts := make([]string, 0, 3)
	for _, p := range []string{buildEnv, region, clientID} {
		p = strings.TrimSpace(p)
		if p == "" {
			p = "default"
		}
		parts = append(parts, p)
	}
	return name + ":" + strings.Join(parts, "/")
}

// discoverCredentials walks the desktop client's auth root and returns every
// usable credential it finds.
//
// It never assumes the <buildEnv>/<region>/<clientId> layout is cn/prod: the
// directory levels are read from the tree itself, so a machine signed in to the
// international build yields the same result without a code change.
func discoverCredentials(root string, logf func(string, ...any)) []credential {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil
	}

	var (
		out   []credential
		seen  = map[string]bool{}
		label = desktopUserLabel()
	)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree is skipped, not fatal
		}
		if d.IsDir() {
			// Bound the walk: the layout is at most a few levels deep, and a
			// deeper tree is not one this module understands.
			if rel, rerr := filepath.Rel(root, path); rerr == nil && rel != "." {
				if strings.Count(filepath.ToSlash(rel), "/") > 3 {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.EqualFold(d.Name(), "auth.json") {
			return nil
		}
		cred, ok := loadCredential(path, label, logf)
		if !ok {
			return nil
		}
		if seen[cred.ID] {
			return nil
		}
		seen[cred.ID] = true
		out = append(out, cred)
		return nil
	})

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// loadCredential reads one auth.json.  It reports false for a file that exists
// but carries no usable token, which is a normal state -- the client writes the
// file before it finishes signing in.
func loadCredential(path, label string, logf func(string, ...any)) (credential, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return credential{}, false
	}
	var doc authFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		logf("minimaxcode: %s is not a readable credential store (%v)", path, err)
		return credential{}, false
	}
	if len(doc.Records) == 0 {
		return credential{}, false
	}

	// The record key contains a literal NUL and an opaque id, so the shape is
	// never assumed: every record is inspected and the best one wins.
	keys := make([]string, 0, len(doc.Records))
	for k := range doc.Records {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var (
		bestKey string
		best    authRecord
	)
	for _, k := range keys {
		r := doc.Records[k]
		if !strings.HasPrefix(strings.TrimSpace(r.AccessToken), accessTokenPrefix) {
			continue
		}
		if bestKey == "" || r.ExpiresAtMs > best.ExpiresAtMs {
			bestKey, best = k, r
		}
	}
	if bestKey == "" {
		// Fall back to any non-empty access token: a future build may drop the
		// prefix, and refusing to relay then would be a worse failure.
		for _, k := range keys {
			if r := doc.Records[k]; strings.TrimSpace(r.AccessToken) != "" {
				if bestKey == "" || r.ExpiresAtMs > best.ExpiresAtMs {
					bestKey, best = k, r
				}
			}
		}
	}
	if bestKey == "" {
		return credential{}, false
	}

	env, region, clientID := credentialLocation(path)
	if clientID == "" {
		clientID = best.ClientID
	}
	if clientID == "" {
		clientID = "default"
	}
	if st, ok := readAuthState(filepath.Join(filepath.Dir(path), "auth-state.json")); ok {
		if env == "" {
			env = st.BuildEnv
		}
		if region == "" {
			region = st.Region
		}
		if st.ClientID != "" {
			clientID = st.ClientID
		}
	}
	if label == "" {
		label = clientID
	}

	return credential{
		ID:         accountID(env, region, clientID),
		Path:       path,
		RecordKey:  bestKey,
		BuildEnv:   env,
		Region:     region,
		ClientID:   clientID,
		Access:     strings.TrimSpace(best.AccessToken),
		Refresh:    strings.TrimSpace(best.RefreshToken),
		ExpiresAt:  msToTime(best.ExpiresAtMs),
		Generation: best.Generation,
		Label:      label,
	}, true
}

// credentialLocation recovers <buildEnv>/<region>/<clientId> from a path.  It
// returns empty strings for levels the path does not have, and the caller fills
// them from auth-state.json.
func credentialLocation(path string) (buildEnv, region, clientID string) {
	dir := filepath.Dir(path)
	clientID = filepath.Base(dir)
	region = filepath.Base(filepath.Dir(dir))
	buildEnv = filepath.Base(filepath.Dir(filepath.Dir(dir)))
	for _, p := range []*string{&buildEnv, &region, &clientID} {
		if *p == "." || *p == string(filepath.Separator) || *p == "" {
			*p = ""
		}
	}
	return buildEnv, region, clientID
}

func readAuthState(path string) (authState, bool) {
	var st authState
	if err := core.ReadJSON(path, &st); err != nil {
		return authState{}, false
	}
	return st, true
}

// desktopUserLabel reads the display name the desktop client shows for the
// signed-in user.  Its absence is not an error: the label is cosmetic.
func desktopUserLabel() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return ""
	}
	// The Electron config is named per build environment; the cn build is the
	// one that has been observed, so it is tried first and any sibling with the
	// same prefix is accepted after it.
	candidates := []string{
		filepath.Join(dir, "MiniMax", "minimax-agent-cn-config.json"),
		filepath.Join(dir, "MiniMax", "minimax-agent-config.json"),
	}
	for _, p := range candidates {
		var doc struct {
			SharedUser struct {
				SubUserName string `json:"subUserName"`
			} `json:"sharedUser"`
		}
		if err := core.ReadJSON(p, &doc); err != nil {
			continue
		}
		if s := strings.TrimSpace(doc.SharedUser.SubUserName); s != "" {
			return s
		}
	}
	return ""
}

func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// oauthRefreshResponse accepts both the standard OAuth2 spelling and the
// camelCase one the desktop client's own store uses, because the successful
// response shape could not be observed and guessing one would be a coin flip.
type oauthRefreshResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	TokenType        string `json:"token_type"`
	AccessTokenCamel string `json:"accessToken"`
	RefreshCamel     string `json:"refreshToken"`
	ExpiresAtMs      int64  `json:"expiresAtMs"`
}

// refreshCredential exchanges a refresh token for a new access token.
//
// The client id is tried in the order the caller supplies: the store's own
// clientId first, then the CLI's, because the two are known to differ and only
// one of them can be right.
func refreshCredential(ctx context.Context, hc *http.Client, tokenURL string, clientIDs []string, refreshToken string) (access, refresh string, expiresAt time.Time, err error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return "", "", time.Time{}, errors.New("minimaxcode: this account has no refresh token")
	}
	if tokenURL == "" {
		return "", "", time.Time{}, errors.New("minimaxcode: no oauth token url is configured")
	}

	var lastErr error
	for _, clientID := range clientIDs {
		clientID = strings.TrimSpace(clientID)
		if clientID == "" {
			continue
		}
		body, merr := marshalNoEscape(map[string]string{
			"grant_type":    "refresh_token",
			"refresh_token": refreshToken,
			"client_id":     clientID,
		})
		if merr != nil {
			return "", "", time.Time{}, merr
		}

		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(string(body)))
		if rerr != nil {
			return "", "", time.Time{}, rerr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")

		resp, derr := hc.Do(req)
		if derr != nil {
			lastErr = derr
			continue
		}
		raw := readLimited(resp.Body, 1<<20)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("minimaxcode: the oauth endpoint answered HTTP %d with client_id %s: %s", resp.StatusCode, clientID, truncate(strings.TrimSpace(string(raw)), 200))
			continue
		}

		var doc oauthRefreshResponse
		if jerr := json.Unmarshal(raw, &doc); jerr != nil {
			lastErr = fmt.Errorf("minimaxcode: the oauth endpoint returned an unreadable body: %w", jerr)
			continue
		}
		access = firstNonEmpty(doc.AccessToken, doc.AccessTokenCamel)
		if access == "" {
			lastErr = fmt.Errorf("minimaxcode: the oauth endpoint answered 200 without an access token")
			continue
		}
		refresh = firstNonEmpty(doc.RefreshToken, doc.RefreshCamel)
		switch {
		case doc.ExpiresAtMs > 0:
			expiresAt = msToTime(doc.ExpiresAtMs)
		case doc.ExpiresIn > 0:
			expiresAt = time.Now().Add(time.Duration(doc.ExpiresIn) * time.Second)
		}
		return access, refresh, expiresAt, nil
	}

	if lastErr == nil {
		lastErr = errors.New("minimaxcode: no oauth client id was available to refresh with")
	}
	return "", "", time.Time{}, lastErr
}

// refreshRejected reports whether the vendor refused the refresh token itself
// rather than failing for a transport or server reason.  MiniMax answers
// invalid_grant, and the description names the remedy ("start a new
// authorization"): the token is retired, not merely stale, so no retry and no
// local bookkeeping can bring it back.  Only a fresh sign-in can.
func refreshRejected(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "invalid_grant") || strings.Contains(s, "no longer be used")
}

// storeCredentialGone reports whether the account's auth.json is present and
// has been emptied, which is exactly the shape a MiniMax Code sign-out leaves
// behind ({"records":{}} with an auth-state of "anonymous").
//
// A missing or malformed file is deliberately NOT reported as gone: a
// transient read must not park an account that is still signed in.
func storeCredentialGone(a Account) bool {
	path := strings.TrimSpace(a.AuthPath)
	if path == "" {
		return false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var doc authFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false
	}
	return len(doc.Records) == 0
}

// readStoreCredential re-reads the desktop client's own store for one account,
// so a caller can notice that the client signed in again or rotated the token
// after this process last loaded it.
func (c *Client) readStoreCredential(a Account) (credential, bool) {
	path := strings.TrimSpace(a.AuthPath)
	if path == "" {
		return credential{}, false
	}
	return loadCredential(path, desktopUserLabel(), c.logf)
}

// writeBackCredential updates the token inside an existing auth.json without
// disturbing anything else in it.
//
// The file is the desktop client's, and this module and that client must agree
// on what it says.  So the update is surgical: the document is decoded as a
// generic map, one record's fields are replaced, and the whole thing is written
// back atomically.  Unknown fields, sibling records, and the NUL-bearing record
// keys all survive untouched.
func writeBackCredential(path, recordKey, access, refresh string, expiresAt time.Time, generation int) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("minimaxcode: %s is not readable JSON: %w", path, err)
	}
	records, ok := doc["records"].(map[string]any)
	if !ok {
		return fmt.Errorf("minimaxcode: %s has no records object", path)
	}
	rec, ok := records[recordKey].(map[string]any)
	if !ok {
		return fmt.Errorf("minimaxcode: %s no longer contains the record this account was read from", path)
	}
	rec["accessToken"] = access
	if strings.TrimSpace(refresh) != "" {
		rec["refreshToken"] = refresh
	}
	if !expiresAt.IsZero() {
		rec["expiresAtMs"] = expiresAt.UnixMilli()
	}
	if generation > 0 {
		rec["generation"] = generation
	}
	records[recordKey] = rec
	doc["records"] = records
	return core.WriteJSONAtomic(path, doc)
}

// credentialTag is a short, irreversible tag for a credential, so the panel can
// show that two rows hold different tokens without either token being visible.
//
// It is deliberately not called "fingerprint": that name belongs to the TLS
// impersonation package, and shadowing it would make the import in this file
// read as a bug.
func credentialTag(secret string) string {
	if secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:4])
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// drainReply reads a probe stream to its end, collecting the reply text.  It is
// bounded in both events and characters: a probe must not be able to run away
// with a request.
func drainReply(s core.Stream) (string, error) {
	if s == nil {
		return "", errors.New("minimaxcode: the probe produced no stream")
	}
	defer s.Close()

	var b strings.Builder
	for i := 0; i < probeEventLimit; i++ {
		ev, err := s.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return strings.TrimSpace(b.String()), err
		}
		switch ev.Type {
		case core.EventDelta:
			if b.Len() < probeReplyLimit {
				b.WriteString(ev.Delta)
			}
		case core.EventDone:
			return strings.TrimSpace(b.String()), nil
		case core.EventError:
			if ev.Err != nil {
				return strings.TrimSpace(b.String()), ev.Err
			}
		}
	}
	return strings.TrimSpace(b.String()), nil
}
