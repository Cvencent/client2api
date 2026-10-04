package codearts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// oauth.go implements the OAuth 2.0 half of the CodeArts credential: the
// PKCE authorisation-code exchange that mints a credential and the
// refresh-token exchange that renews one.
//
// Both are POSTs to the same STS endpoint, both carry a DPoP proof, and both
// are form-encoded.  The two grants are the only difference.
//
// Endpoint and constants, from the reference implementation:
//
//	CLIENT_ID          codearts-agent
//	STS endpoint       https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens
//	redirect path      /oauth/callback
//	grants             authorization_code | refresh_token
//
// The response is a `credentials` object holding the temporary AK/SK/security
// token triple plus an ISO expiry, and — for the authorisation-code grant —
// a refresh token.

const (
	// oauthClientID is the public client identifier the CodeArts portal and
	// the STS endpoint both expect.  It is not a secret.
	oauthClientID = "codearts-agent"
	// oauthRedirectPath is the path the portal redirects back to.
	oauthRedirectPath = "/oauth/callback"
	// oauthRedirectScheme is the custom URI scheme the portal is told to use.
	oauthRedirectScheme = "codearts-agent"

	// stsTokenEndpoint is the IAM token service in cn-north-4.
	stsTokenEndpoint = "https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens"
	// tokenTimeout bounds one token exchange.
	tokenTimeout = 60 * time.Second
)

// tokenResponse is the STS answer, for both grants.
//
// A failure is reported inside a 200 body as often as it is by a status code,
// so the error fields are part of the shape rather than an error return.
type tokenResponse struct {
	Credentials *tokenCredentials `json:"credentials"`
	// RefreshToken is present on the authorisation-code grant only; a refresh
	// that does not return one must keep the previous value.
	RefreshToken string `json:"refresh_token"`

	Error     string `json:"error"`
	ErrorCode string `json:"error_code"`
	ErrorMsg  string `json:"error_msg"`
}

// tokenCredentials is the temporary AK/SK/security-token triple.
type tokenCredentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SecurityToken   string `json:"security_token"`
	Expiration      string `json:"expiration"`
}

// errRefreshTerminal marks a refresh failure that no retry can fix: the
// refresh token itself is dead and the operator has to sign in again.
//
// The reference implementation classifies exactly two conditions this way —
// `error == "invalid_grant"` and an `error_code` containing
// `ExpiredRefreshToken` — and deliberately does NOT include
// `InvalidDPoPHeader`, which is a per-request rejection rather than evidence
// that the refresh token is gone.  Getting that wrong would mark a perfectly
// good account dead because one proof was malformed.
var errRefreshTerminal = errors.New("codearts: the refresh token is no longer valid; sign in again")

// isTerminalRefreshFailure reports whether a failed token response means the
// refresh token is permanently gone.
func isTerminalRefreshFailure(resp *tokenResponse) bool {
	if resp == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(resp.Error), "invalid_grant") {
		return true
	}
	return strings.Contains(resp.ErrorCode, "ExpiredRefreshToken")
}

// tokenErrorText renders the failure the server described.
func tokenErrorText(resp *tokenResponse) string {
	if resp == nil {
		return "the token endpoint returned an empty body"
	}
	parts := make([]string, 0, 3)
	if v := strings.TrimSpace(resp.Error); v != "" {
		parts = append(parts, v)
	}
	if v := strings.TrimSpace(resp.ErrorCode); v != "" {
		parts = append(parts, v)
	}
	if v := strings.TrimSpace(resp.ErrorMsg); v != "" {
		parts = append(parts, v)
	}
	if len(parts) == 0 {
		return "the token endpoint refused the request without saying why"
	}
	return strings.Join(parts, ": ")
}

// tokenRequest is one POST to the STS endpoint.
type tokenRequest struct {
	form url.Values
	// terminalErr, when non-nil, is returned instead of a generic failure if
	// the server classifies the response as a dead refresh token.  Only the
	// refresh grant sets it.
	terminalErr error
}

// exchangeAuthorizationCode redeems the code the portal redirected back with.
//
// `redirectURI` must be byte-for-byte the value that was sent as the
// authorisation request's redirect target, because the server compares them.
func (c *Client) exchangeAuthorizationCode(ctx context.Context, code, verifier, redirectURI string, key *dpopKeyPair) (account, error) {
	form := url.Values{}
	form.Set("client_id", oauthClientID)
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("redirect_uri", redirectURI)

	resp, err := c.postToken(ctx, form, key, nil)
	if err != nil {
		return account{}, err
	}
	return credentialFromTokenResponse(resp, verifier, key)
}

// exchangeRefreshToken renews a credential that is still valid.
//
// `verifier` is the PKCE verifier the credential was minted with.  The token
// endpoint requires it even on the refresh grant, which is unusual enough to
// be worth stating: a credential stored without its verifier cannot be
// refreshed, and the reference implementation persists it alongside the
// refresh token for exactly this reason.
func (c *Client) exchangeRefreshToken(ctx context.Context, refreshToken, verifier string, key *dpopKeyPair) (account, error) {
	form := url.Values{}
	form.Set("client_id", oauthClientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("code_verifier", verifier)

	resp, err := c.postToken(ctx, form, key, errRefreshTerminal)
	if err != nil {
		return account{}, err
	}
	return credentialFromTokenResponse(resp, verifier, key)
}

// postToken performs one signed-with-DPoP form POST and decodes the answer.
//
// A non-2xx status is read before it is judged: the STS endpoint reports a
// dead refresh token with a 400 and a JSON body, so throwing the body away
// would lose the one fact the caller needs.
func (c *Client) postToken(ctx context.Context, form url.Values, key *dpopKeyPair, terminalErr error) (*tokenResponse, error) {
	if key == nil {
		return nil, errors.New("codearts: the token endpoint needs a DPoP key")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, tokenTimeout)
	defer cancel()

	body := form.Encode()
	endpoint := c.cfg.stsURL()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("codearts: building the token request: %w", err)
	}
	// The DPoP proof names the exact URL it is for, so the htu must be the
	// endpoint actually called, not the built-in default.
	proof, err := key.signDpopProof(http.MethodPost, endpoint, time.Now())
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("DPoP", proof.Compact)
	req.Header.Set("User-Agent", c.cfg.userAgent())

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("codearts: calling the token endpoint: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("codearts: reading the token response: %w", err)
	}
	var out tokenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("codearts: the token endpoint answered HTTP %d with a body that is not JSON: %s",
			resp.StatusCode, cleanErrorText(string(raw)))
	}
	if out.Credentials == nil {
		if terminalErr != nil && isTerminalRefreshFailure(&out) {
			return nil, fmt.Errorf("%w (%s)", terminalErr, tokenErrorText(&out))
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("codearts: the token endpoint answered HTTP %d: %s", resp.StatusCode, tokenErrorText(&out))
		}
		return nil, fmt.Errorf("codearts: the token endpoint returned no credentials: %s", tokenErrorText(&out))
	}
	return &out, nil
}

// credentialFromTokenResponse turns an STS answer into a stored credential.
func credentialFromTokenResponse(resp *tokenResponse, verifier string, key *dpopKeyPair) (account, error) {
	if resp == nil || resp.Credentials == nil {
		return account{}, errors.New("codearts: the token endpoint returned no credentials")
	}
	creds := resp.Credentials
	a := account{
		AccessKeyID:     strings.TrimSpace(creds.AccessKeyID),
		SecretAccessKey: strings.TrimSpace(creds.SecretAccessKey),
		SecurityToken:   strings.TrimSpace(creds.SecurityToken),
		RefreshToken:    strings.TrimSpace(resp.RefreshToken),
		ExpiresAt:       parseExpiry(creds.Expiration),
		CodeVerifier:    verifier,
	}
	if key != nil {
		a.DpopPrivateJwk = key.PrivateJwk
	}
	if a.AccessKeyID == "" || a.SecretAccessKey == "" {
		return account{}, errors.New("codearts: the token endpoint returned a credential without an access key")
	}
	if a.SecurityToken == "" {
		// A credential with no security token cannot sign a chat request at
		// all, so it is refused at the point it is minted rather than saved
		// and discovered later.
		return account{}, errors.New("codearts: the token endpoint returned a credential without a security token")
	}
	if a.ExpiresAt == 0 {
		// The vendor always sends an expiry; if it ever stops, assume the
		// reference implementation's own fallback of 24 hours rather than
		// treating the credential as valid forever.
		a.ExpiresAt = time.Now().Add(24 * time.Hour).Unix()
	}
	a.ID = a.accountID()
	return a, nil
}

// ---------------------------------------------------------------------------
// authorisation URL
// ---------------------------------------------------------------------------

// Portal endpoints for the browser login flow.
const (
	// portalAuthorizeBase is where the operator is sent to sign in.
	portalAuthorizeBase = "https://codearts.huaweicloud.com/portal/authorize"
	// portalLoginBase is where the portal is told the sign-in finished, so
	// the browser tab can show a "you may close this window" page.
	portalLoginBase = "https://codearts.huaweicloud.com/portal/login"
	// oauthTheme / oauthLocale are the portal's own parameters.
	oauthTheme  = "2"
	oauthLocale = "zh-cn"
	// loginPluginName / loginPluginVersion identify the IDE extension the
	// portal is signing in for.  The version is the real published extension
	// version and is NOT this program's version; the portal validates it.
	loginPluginName    = "snap_AIIDE"
	loginPluginVersion = "5.2.0"
	// ticketPluginName / ticketPluginVersion are the older plugin identity,
	// used by the legacy ticket flow.
	ticketPluginName    = "snap_jetbrains"
	ticketPluginVersion = "26.3.3"
)

// buildAuthorizeURL is the portal URL the operator opens in a browser.
//
// Three details are load-bearing, and all three were measured in the
// reference implementation:
//
//   - `code_challenge_method` is the literal `SHA-256`.  The RFC 7636 value is
//     `S256`; sending that makes the portal silently fall back to the legacy
//     ticket flow instead of the OAuth code flow.
//   - the URL must NOT contain `auth_callback_url`; the portal treats that as
//     an abnormal request and also falls back.
//   - `port` must be >= 10000.  The portal refuses to redirect to a low port,
//     so the caller has to have bound one in that range.
func buildAuthorizeURL(port int, challenge, ticketID string) string {
	q := url.Values{}
	q.Set("theme", oauthTheme)
	q.Set("locale", oauthLocale)
	q.Set("uri_scheme", oauthRedirectScheme)
	q.Set("client_id", oauthClientID)
	q.Set("port", fmt.Sprintf("%d", port))
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "SHA-256")
	if ticketID != "" {
		q.Set("ticket_id", ticketID)
	}
	q.Set("plugin-name", loginPluginName)
	q.Set("plugin-version", loginPluginVersion)
	return portalAuthorizeBase + "?" + q.Encode()
}

// buildPortalLoginResultURL is where the browser is redirected once the code
// has been redeemed, so the tab ends somewhere sensible.
func buildPortalLoginResultURL(ok bool) string {
	q := url.Values{}
	q.Set("login_succeed", fmt.Sprintf("%t", ok))
	q.Set("uri_scheme", oauthRedirectScheme)
	q.Set("locale", oauthLocale)
	return portalLoginBase + "?" + q.Encode()
}
