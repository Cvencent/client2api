package cline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"client2api/internal/core"
)

// Endpoint paths, relative to the configured origins.
const (
	chatPath               = "/api/v1/chat/completions"
	modelsPath             = "/api/v1/models"
	recommendedPath        = "/api/v1/ai/cline/recommended-models"
	registerPath           = "/api/v1/auth/register"
	refreshPath            = "/api/v1/auth/refresh"
	userInfoPath           = "/api/v1/users/me"
	workOSDevicePath       = "/user_management/authorize/device"
	workOSAuthenticatePath = "/user_management/authenticate"
)

// maxAuthBodyBytes bounds every account/authorisation response body.
const maxAuthBodyBytes = 1 << 20

// errEmptyGrant is returned when a token endpoint answered 200 with no token.
var errEmptyGrant = errors.New("cline: token endpoint returned no access token")

// --- request plumbing ------------------------------------------------------

// applyClientHeaders sets the four headers the official client sends on every
// request, inference and account alike.
func applyClientHeaders(req *http.Request) {
	req.Header.Set("HTTP-Referer", headerReferer)
	req.Header.Set("X-Title", headerTitle)
	req.Header.Set("X-IS-MULTIROOT", headerMultiRoot)
	req.Header.Set("X-CLIENT-TYPE", headerClientType)
}

// doJSON performs one JSON request and returns the response status, headers and
// body.  A transport failure is returned as an error; an HTTP status is not.
func (c *Client) doJSON(ctx context.Context, method, endpoint, bearer string, payload any) (int, http.Header, []byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return 0, nil, nil, err
	}
	applyClientHeaders(req)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		// The workos: prefix is part of the credential.  Stripping it here is
		// what produces the misleading 401 the README warns about.
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body, maxAuthBodyBytes)
	return resp.StatusCode, resp.Header, raw, nil
}

// --- token endpoints -------------------------------------------------------

// decodeGrant validates and decodes a register/refresh envelope.
//
// The success test is deliberately `success && data.accessToken`: the response
// is wrapped, and testing a bare accessToken would accept a {"success":false}
// envelope that happens to carry a stale field.
func decodeGrant(raw []byte) (tokenGrant, error) {
	var env grantEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return tokenGrant{}, fmt.Errorf("cline: unreadable token response: %w", err)
	}
	var grant tokenGrant
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &grant); err != nil {
			return tokenGrant{}, fmt.Errorf("cline: unreadable token response: %w", err)
		}
	}
	if env.Success == nil || !*env.Success {
		msg := cleanErrorText(firstNonEmpty(env.Message, errorMessage(string(env.Error))))
		if msg == "" {
			msg = "the token endpoint reported failure"
		}
		return tokenGrant{}, errors.New("cline: " + msg)
	}
	if strings.TrimSpace(grant.AccessToken) == "" {
		return tokenGrant{}, errEmptyGrant
	}
	return grant, nil
}

// refresh exchanges a refresh token for a fresh access token.
//
// The body is camelCase -- {"refreshToken":…,"grantType":"refresh_token"} -- NOT
// the OAuth-standard snake_case.  Both fields are required, and a wrong field
// name comes back as a generic auth failure rather than a missing-field error.
func (c *Client) refresh(ctx context.Context, refreshToken string) (tokenGrant, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return tokenGrant{}, errors.New("cline: no refresh token")
	}
	payload := map[string]any{
		"refreshToken": refreshToken,
		"grantType":    "refresh_token",
	}
	status, _, raw, err := c.doJSON(ctx, http.MethodPost, c.cfg.endpoint(refreshPath), "", payload)
	if err != nil {
		return tokenGrant{}, err
	}
	if status != http.StatusOK {
		return tokenGrant{}, errors.New("cline: refresh rejected: " + errorMessage(string(raw)))
	}
	return decodeGrant(raw)
}

// register exchanges WorkOS tokens for Cline's own credential.  It is the last
// step of the device flow.
func (c *Client) register(ctx context.Context, workOSAccessToken, workOSRefreshToken string) (tokenGrant, error) {
	payload := map[string]any{}
	if v := strings.TrimSpace(workOSAccessToken); v != "" {
		payload["accessToken"] = v
		payload["token"] = v
	}
	if v := strings.TrimSpace(workOSRefreshToken); v != "" {
		payload["refreshToken"] = v
	}
	if id := c.cfg.workOSClientID(); id != "" {
		payload["clientId"] = id
	}
	status, _, raw, err := c.doJSON(ctx, http.MethodPost, c.cfg.endpoint(registerPath), "", payload)
	if err != nil {
		return tokenGrant{}, err
	}
	if status != http.StatusOK {
		return tokenGrant{}, errors.New("cline: registration rejected: " + errorMessage(string(raw)))
	}
	return decodeGrant(raw)
}

// --- WorkOS device flow ----------------------------------------------------

// deviceAuthResponse is the body of POST {workos}/user_management/authorize/device.
// WorkOS documents snake_case while its newer responses use camelCase, so both
// spellings are accepted.
type deviceAuthResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
	Error                   string `json:"error"`
	ErrorDescription        string `json:"error_description"`
}

// startDeviceAuth opens a device-authorisation flow and returns the record the
// human is expected to act on.
func (c *Client) startDeviceAuth(ctx context.Context) (*loginRecord, error) {
	form := url.Values{}
	form.Set("client_id", c.cfg.workOSClientID())
	form.Set("scope", "openid profile email")
	status, _, raw, err := c.postForm(ctx, c.cfg.workOSEndpoint(workOSDevicePath), form)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, errors.New("cline: device authorisation refused: " + errorMessage(string(raw)))
	}
	var body deviceAuthResponse
	if err := json.Unmarshal(raw, &body); err != nil {
		// The endpoint is documented as form-encoded but answers JSON; fall
		// back to a form parse rather than failing on a shape difference.
		if vals, verr := url.ParseQuery(string(raw)); verr == nil {
			body = deviceAuthResponse{
				DeviceCode:              vals.Get("device_code"),
				UserCode:                vals.Get("user_code"),
				VerificationURI:         vals.Get("verification_uri"),
				VerificationURIComplete: vals.Get("verification_uri_complete"),
			}
		} else {
			return nil, fmt.Errorf("cline: unreadable device response: %w", err)
		}
	}
	// Accept the camelCase spellings too.
	if body.DeviceCode == "" || body.UserCode == "" || body.VerificationURI == "" {
		var alt map[string]any
		if json.Unmarshal(raw, &alt) == nil {
			body.DeviceCode = firstNonEmpty(body.DeviceCode, pickString(alt, "deviceCode"))
			body.UserCode = firstNonEmpty(body.UserCode, pickString(alt, "userCode"))
			body.VerificationURI = firstNonEmpty(body.VerificationURI,
				pickString(alt, "verificationUri", "verificationUrl", "verificationURI"))
			body.VerificationURIComplete = firstNonEmpty(body.VerificationURIComplete,
				pickString(alt, "verificationUriComplete", "verificationUrlComplete"))
			if body.ExpiresIn == 0 {
				if n, ok := asInt(alt["expiresIn"]); ok {
					body.ExpiresIn = int64(n)
				}
			}
			if body.Interval == 0 {
				if n, ok := asInt(alt["interval"]); ok {
					body.Interval = int64(n)
				}
			}
		}
	}
	if body.DeviceCode == "" {
		msg := cleanErrorText(firstNonEmpty(body.ErrorDescription, body.Error))
		if msg == "" {
			msg = "the device endpoint returned no device code"
		}
		return nil, errors.New("cline: " + msg)
	}
	now := time.Now()
	url := firstNonEmpty(body.VerificationURIComplete, body.VerificationURI)
	rec := &loginRecord{
		URL:        url,
		UserCode:   body.UserCode,
		DeviceCode: body.DeviceCode,
		Interval:   body.Interval,
		StartedAt:  now.Unix(),
	}
	if body.ExpiresIn > 0 {
		rec.ExpiresAt = now.Add(time.Duration(body.ExpiresIn) * time.Second).Unix()
	}
	if rec.URL == "" {
		rec.URL = c.cfg.appBase()
	}
	return rec, nil
}

// postForm sends a form-encoded POST.  The WorkOS endpoints are documented as
// application/x-www-form-urlencoded, unlike the Cline JSON endpoints.
func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, nil, err
	}
	applyClientHeaders(req)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body, maxAuthBodyBytes)
	return resp.StatusCode, resp.Header, raw, nil
}

// workOSTokens are the tokens the authenticate endpoint mints.
type workOSTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// pollDeviceAuth asks whether the human has authorised yet.  The bool reports
// whether the flow is still pending; a false return with a nil error means the
// tokens were minted.
func (c *Client) pollDeviceAuth(ctx context.Context, deviceCode string) (workOSTokens, bool, error) {
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	form.Set("device_code", deviceCode)
	form.Set("client_id", c.cfg.workOSClientID())
	status, _, raw, err := c.postForm(ctx, c.cfg.workOSEndpoint(workOSAuthenticatePath), form)
	if err != nil {
		return workOSTokens{}, false, err
	}
	var toks workOSTokens
	if jerr := json.Unmarshal(raw, &toks); jerr != nil {
		if vals, verr := url.ParseQuery(string(raw)); verr == nil {
			toks = workOSTokens{
				AccessToken:  vals.Get("access_token"),
				RefreshToken: vals.Get("refresh_token"),
				Error:        vals.Get("error"),
				ErrorDesc:    vals.Get("error_description"),
			}
		} else {
			return workOSTokens{}, false, fmt.Errorf("cline: unreadable authorisation response: %w", jerr)
		}
	}
	if toks.Error != "" {
		switch toks.Error {
		case "authorization_pending":
			return workOSTokens{}, true, nil
		case "slow_down":
			return workOSTokens{}, true, nil
		case "expired_token":
			return workOSTokens{}, false, errors.New("cline: the device code expired before it was authorised")
		case "access_denied":
			return workOSTokens{}, false, errors.New("cline: the authorisation request was denied")
		default:
			msg := cleanErrorText(firstNonEmpty(toks.ErrorDesc, toks.Error))
			return workOSTokens{}, false, errors.New("cline: authorisation failed: " + msg)
		}
	}
	if status != http.StatusOK || strings.TrimSpace(toks.AccessToken) == "" {
		return workOSTokens{}, false, errors.New("cline: authorisation failed: " + errorMessage(string(raw)))
	}
	return toks, false, nil
}

// RunDeviceFlow performs the whole WorkOS device-authorisation flow and stores
// the resulting Cline credential in accounts.json.
//
// It is exported and self-contained so a future `client2api -login cline` can
// call it with the same core.Deps.  It prints the URL the human must open; it
// needs a human, and cannot complete on its own.
func RunDeviceFlow(ctx context.Context, deps core.Deps, out io.Writer) error {
	cfg, err := parseConfig(deps.Config)
	if err != nil {
		deps.Log("cline: invalid config, using defaults: %v", err)
		cfg = config{}
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.loginTimeout())
		defer cancel()
	}
	if out == nil {
		out = io.Discard
	}
	c := &Client{deps: deps, cfg: cfg}
	c.setPaths()

	rec, err := c.startDeviceAuth(ctx)
	if err != nil {
		return err
	}
	if c.loginPath != "" {
		if werr := core.WriteJSONAtomic(c.loginPath, rec); werr != nil {
			deps.Log("cline: could not record the pending device flow: %v", werr)
		}
	}
	fmt.Fprintf(out, "cline: open this URL in a browser and authorise access:\n  %s\n", rec.URL)
	if rec.UserCode != "" {
		fmt.Fprintf(out, "cline: confirm the code %s\n", rec.UserCode)
	}

	interval := rec.pollEvery(cfg.pollInterval())
	for {
		if !core.SleepCtx(ctx, interval) {
			return fmt.Errorf("cline: device authorisation was not completed in time: %w", ctx.Err())
		}
		toks, pending, err := c.pollDeviceAuth(ctx, rec.DeviceCode)
		if err != nil {
			return err
		}
		if pending {
			continue
		}
		grant, err := c.register(ctx, toks.AccessToken, toks.RefreshToken)
		if err != nil {
			return err
		}
		acct := accountFromGrant(account{}, grant)
		if acct.RefreshToken == "" {
			acct.RefreshToken = toks.RefreshToken
		}
		if acct.AccessToken == "" {
			return errEmptyGrant
		}
		if err := upsertStoredAccount(c.accountsPath, acct); err != nil {
			return err
		}
		if c.loginPath != "" {
			_ = os.Remove(c.loginPath)
		}
		fmt.Fprintf(out, "cline: authorised %s; credential stored\n", acct.label())
		return nil
	}
}

// upsertStoredAccount merges an account into accounts.json, clearing any old
// verdict: a fresh credential must never be shadowed by a stale failure.
func upsertStoredAccount(path string, a account) error {
	if path == "" {
		return errors.New("cline: no data directory to store the credential in")
	}
	var sf storeFile
	if err := core.ReadJSON(path, &sf); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	merged := false
	for i := range sf.Accounts {
		if sameAccount(sf.Accounts[i], a) {
			keep := sf.Accounts[i]
			a.CreatedAt = keep.CreatedAt
			a.LastUsed = keep.LastUsed
			sf.Accounts[i] = a
			merged = true
			break
		}
	}
	if !merged {
		a.CreatedAt = time.Now().Unix()
		sf.Accounts = append(sf.Accounts, a)
	}
	return core.WriteJSONAtomic(path, sf)
}

// sameAccount reports whether two records describe the same vendor account.
func sameAccount(a, b account) bool {
	if a.AccountID != "" && b.AccountID != "" && a.AccountID == b.AccountID {
		return true
	}
	if a.ID != "" && b.ID != "" && a.ID == b.ID {
		return true
	}
	if a.Email != "" && b.Email != "" && strings.EqualFold(a.Email, b.Email) {
		return true
	}
	if a.AccessToken != "" && a.AccessToken == b.AccessToken {
		return true
	}
	return false
}

// loginState reads a pending device flow, or nil when there is none.
func (c *Client) loginState() *loginRecord {
	if c.loginPath == "" {
		return nil
	}
	var rec loginRecord
	if err := core.ReadJSON(c.loginPath, &rec); err != nil {
		return nil
	}
	if rec.expired(time.Now()) {
		return nil
	}
	return &rec
}
