package kimi

// The module's own token store.
//
// Before web login existed, this module owned no credential at all: the CLI's
// login was the only account, and the module could merely observe that a
// credential file existed.  A panel-driven device login produces a token the
// module itself obtained, so it needs somewhere to keep it.
//
// The contract is explicit that a module's state lives in its DataDir and
// nowhere else (internal/core/core.go: "Modules must keep all of their state
// inside it and nowhere else"), so the token goes to
// <DataDir>/kimi-token.json.  That file is deliberately *not* the CLI's own
// credential file: writing into ~/.kimi-code would be this module reaching
// outside its sandbox and mutating another program's login.
//
// The on-disk shape mirrors the CLI's own credentials file (snake_case,
// expires_at as unix seconds) so the two are recognisable side by side, but the
// two stores are independent and neither is derived from the other.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"client2api/internal/core"
)

const (
	// tokenFileName lives inside the module's DataDir.
	tokenFileName = "kimi-token.json"
	// tokenFileMode is 0600: core.WriteJSONAtomic enforces this.
	tokenFileMode = 0o600
)

// storedToken is the persisted device-flow grant.  The field names match the
// CLI's credentials file on purpose; the values never leave this module.
type storedToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    int64     `json:"expires_at,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	OAuthHost    string    `json:"oauth_host,omitempty"`
	ObtainedAt   time.Time `json:"obtained_at,omitempty"`
}

// expired reports whether the access token is past its stated expiry.  A token
// with no expiry is never reported as expired: the vendor is the authority on
// whether it still works, and guessing "expired" would break a working account.
func (t storedToken) expired() bool {
	if t.ExpiresAt <= 0 {
		return false
	}
	return time.Now().After(time.Unix(t.ExpiresAt, 0))
}

// expiryRFC3339 renders the expiry for the panel, or "" when the vendor did not
// state one.
func (t storedToken) expiryRFC3339() string {
	if t.ExpiresAt <= 0 {
		return ""
	}
	return time.Unix(t.ExpiresAt, 0).UTC().Format(time.RFC3339)
}

// usable reports whether there is something to send upstream.
func (t storedToken) usable() bool {
	return strings.TrimSpace(t.AccessToken) != ""
}

// tokenRefreshSkew is how far ahead of the stated expiry a grant is renewed.
// The device flow's access token lives 30 minutes, so renewing a little early
// costs one request and avoids a request that is already doomed in flight.  It
// is a var so a test can shrink it instead of waiting out a real token.
var tokenRefreshSkew = 5 * time.Minute

// needsRefresh reports whether the grant is worth renewing before it is used.
//
// A token with no stated expiry is never renewed: the vendor is the authority
// on whether it still works, and a token with no expiry has no deadline to
// anticipate.
func (t storedToken) needsRefresh() bool {
	if t.ExpiresAt <= 0 {
		return false
	}
	return time.Now().Add(tokenRefreshSkew).After(time.Unix(t.ExpiresAt, 0))
}

// freshToken returns the grant to send upstream, renewing it with the stored
// refresh token when it is at or near its stated expiry.
//
// Renewal is best effort and never turns a usable account into an unusable one:
// a failed renewal still returns the stored grant, because only the vendor can
// say whether it is actually dead.  The error is returned so the caller can
// explain *why* the credential will stop working, not to abort the request.
func (c *Client) freshToken(ctx context.Context) (storedToken, bool, error) {
	tok, ok := c.loadToken()
	if !ok {
		return storedToken{}, false, nil
	}
	if !tok.needsRefresh() || strings.TrimSpace(tok.RefreshToken) == "" {
		return tok, true, nil
	}
	renewed, err := c.renewToken(ctx, tok)
	if err != nil {
		return tok, true, err
	}
	return renewed, true, nil
}

// tokenPath is the store location, or "" when the core gave us no DataDir.
func (c *Client) tokenPath() string {
	dir := strings.TrimSpace(c.cfg.dataDir)
	if dir == "" {
		return ""
	}
	return dir + string(os.PathSeparator) + tokenFileName
}

// loadToken reads the stored grant.  A missing or corrupt file is reported as
// "no token", never as an error: an unreadable credential must degrade to "log
// in again", not to a module that refuses to start.
func (c *Client) loadToken() (storedToken, bool) {
	var tok storedToken

	path := c.tokenPath()
	if path == "" {
		return tok, false
	}
	if err := core.ReadJSON(path, &tok); err != nil {
		return storedToken{}, false
	}
	if !tok.usable() {
		return storedToken{}, false
	}
	return tok, true
}

// saveToken writes the grant atomically at 0600.
func (c *Client) saveToken(tok storedToken) error {
	path := c.tokenPath()
	if path == "" {
		return fmt.Errorf("no data directory is configured, so the token has nowhere to live")
	}
	if !tok.usable() {
		return fmt.Errorf("refusing to store an empty access token")
	}
	if err := core.WriteJSONAtomic(path, tok); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// clearToken removes the stored grant.  Removing an already-absent file is not
// an error: the caller asked for the end state, not for a specific transition.
func (c *Client) clearToken() error {
	path := c.tokenPath()
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}

// tokenStatus is the panel-facing summary of the stored grant.  It deliberately
// exposes no part of the secret.
func (c *Client) tokenStatus() (core.AccountStatus, bool) {
	tok, ok := c.loadToken()
	if !ok {
		return core.AccountStatus{}, false
	}
	state := "ready"
	note := "signed in from the panel; this module calls the vendor directly over HTTPS"
	switch {
	case !tok.expired():
		// The note above already says everything there is to say.
	case strings.TrimSpace(tok.RefreshToken) != "":
		// The access token is short lived by design; the refresh token is not.
		// Calling this account broken would tell the operator to sign in again
		// for something the module renews by itself on the next request.
		note = "the stored access token has expired and is renewed from the saved refresh token on the next request"
	default:
		state = "invalid"
		note = "the stored access token has expired and no refresh token was saved with it; sign in again from the panel"
	}
	return core.AccountStatus{
		ID:        webLoginID,
		Label:     "Kimi Code (web login)",
		Enabled:   true,
		State:     state,
		ExpiresAt: tok.expiryRFC3339(),
		Note:      note,
		Identity:  core.JWTIdentity(tok.AccessToken),
		Extra: map[string]any{
			"kind":       "oauth",
			"oauth_host": tok.OAuthHost,
			"stored_at":  c.tokenPath(),
		},
	}, true
}
