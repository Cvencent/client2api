package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Overseas account activation (the "register" wizard), ported from the
// reference internal/upstream/global_register.go (MIT).
//
// This one flow is deliberately *not* the CLI identity: the console wizard is a
// browser surface, so the reference sends a desktop-browser user agent, an
// Origin/Referer pair and a bearer token, and reads a bare `{code,msg,data}`
// envelope without the gateway's usual wrapper.  Realm guard: every entry point
// refuses a non-global account up front, because none of these routes exist on
// the CN realm.

// globalWebUA is the browser identity the console wizard expects.
const globalWebUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// intlWhitelist is the country ordering the overseas realm accepts, in the
// order the reference filters them.  Keyed by IOS2.
var intlWhitelist = []string{"HK", "MO", "SG", "TH", "PH", "MY", "ID"}

// globalRegisterPathList is the country catalogue route.
const globalRegisterCountriesPath = "/billing/area/get-country-code"

// globalRegisterStatusPath is the activation-status route; userId is appended
// as a query parameter.
const globalRegisterStatusPath = "/auth/realms/copilot/overseas/user/register"

// globalSubmitRegionPath is the console login/account route that records region.
const globalSubmitRegionPath = "/console/login/account"

// GlobalCountry is one entry of the console's country catalogue.
type GlobalCountry struct {
	EnName string `json:"EnName"`
	Name   string `json:"Name"`
	IOS2   string `json:"IOS2"`
	IOS3   string `json:"IOS3"`
	Code   string `json:"Code"`
}

// globalRegisterBase is the web base for the register wizard.  The reference's
// globalRegisterBase (internal/upstream/global_register.go:44) rides the
// billing host, not the chat host, so an operator who overrides
// `billing_base_global` moves the wizard with it.
func (c *Client) globalRegisterBase() string {
	if c != nil && c.up != nil {
		return c.up.globalBillingBase()
	}
	return defaultGlobalBase
}

// globalRegisterReq builds one wizard request.  It carries the browser identity,
// the Origin/Referer pair and, when token is set, the bearer token.
func (c *Client) globalRegisterReq(ctx context.Context, method, url, token string, payload any) (*http.Request, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	base := c.globalRegisterBase()
	ua := globalWebUA
	if c.up != nil && strings.TrimSpace(c.up.UserAgent) != "" {
		ua = strings.TrimSpace(c.up.UserAgent)
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", base)
	req.Header.Set("Referer", base+"/")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

// globalRegisterJSON performs a wizard request and unwraps the bare envelope.
func (c *Client) globalRegisterJSON(req *http.Request) (int, string, json.RawMessage, error) {
	hc := c.up.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if err != nil {
		return 0, "", nil, fmt.Errorf("read body: %w", err)
	}
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, "", nil, fmt.Errorf("global register parse: %w", err)
	}
	return env.Code, env.Msg, env.Data, nil
}

// GlobalFetchCountries lists the regions the overseas realm accepts.  intlOnly
// keeps just the whitelist, in the reference's order, because the console offers
// far more countries than the copilot realm will actually activate.
func (c *Client) GlobalFetchCountries(ctx context.Context, a *Auth, intlOnly bool) ([]GlobalCountry, error) {
	if a == nil || !a.IsGlobal() {
		return nil, errors.New("fetch countries: only global accounts")
	}
	req, err := c.globalRegisterReq(ctx, http.MethodPost,
		c.globalRegisterBase()+globalRegisterCountriesPath, a.AccessTokenValue(),
		map[string]any{"filterForbidden": 1})
	if err != nil {
		return nil, err
	}
	code, msg, data, err := c.globalRegisterJSON(req)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("get-country-code: %s (code=%d)", msg, code)
	}
	// The catalogue is sometimes double-enveloped: data is a JSON *string*
	// holding another {data:{list:[...]}} document.
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "\"") {
		var inner string
		if err := json.Unmarshal(data, &inner); err != nil {
			return nil, fmt.Errorf("get-country-code: unwrap data: %w", err)
		}
		data = json.RawMessage(inner)
	}
	var payload struct {
		Data struct {
			List []GlobalCountry `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("get-country-code: parse: %w", err)
	}
	list := payload.Data.List
	if !intlOnly {
		return list, nil
	}
	byCode := make(map[string]GlobalCountry, len(list))
	for _, c := range list {
		byCode[strings.ToUpper(strings.TrimSpace(c.IOS2))] = c
	}
	out := make([]GlobalCountry, 0, len(intlWhitelist))
	for _, want := range intlWhitelist {
		if c, ok := byCode[want]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// GlobalRegisterStatus reports whether the overseas account is activated, and
// whether activation is waiting on a region choice.
func (c *Client) GlobalRegisterStatus(ctx context.Context, a *Auth) (activated bool, needsRegion bool, msg string, err error) {
	if a == nil || !a.IsGlobal() {
		return false, false, "", errors.New("register status: only global accounts")
	}
	uid := a.UIDValue()
	url := c.globalRegisterBase() + globalRegisterStatusPath + "?userId=" + uid
	req, err := c.globalRegisterReq(ctx, http.MethodGet, url, a.AccessTokenValue(), nil)
	if err != nil {
		return false, false, "", err
	}
	if uid != "" {
		req.Header.Set("X-User-Id", uid)
	}
	code, m, _, err := c.globalRegisterJSON(req)
	if err != nil {
		return false, false, "", err
	}
	switch {
	case code == 200:
		return true, false, "register success", nil
	case code == 500 || strings.Contains(strings.ToLower(m), "region required"):
		return false, true, m, nil
	default:
		return false, false, m, nil
	}
}

// GlobalSubmitRegion records the chosen region.  Every attribute is a
// one-element array — the console posts them the way an HTML form would, and a
// bare string is rejected.
func (c *Client) GlobalSubmitRegion(ctx context.Context, a *Auth, country GlobalCountry) error {
	if a == nil || !a.IsGlobal() {
		return errors.New("submit region: only global accounts")
	}
	payload := map[string]any{
		"attributes": map[string]any{
			"countryCode":     []string{country.Code},
			"countryFullName": []string{country.EnName},
			"countryName":     []string{country.IOS2},
		},
	}
	req, err := c.globalRegisterReq(ctx, http.MethodPost,
		c.globalRegisterBase()+globalSubmitRegionPath, a.AccessTokenValue(), payload)
	if err != nil {
		return err
	}
	code, msg, _, err := c.globalRegisterJSON(req)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("submit region: %s (code=%d)", msg, code)
	}
	return nil
}

// GlobalCompleteRegistration is the one-shot repair: check, submit the first
// whitelisted region when the account is waiting on one, and re-check.  It is
// idempotent and deliberately non-fatal to its caller: an account that is
// already activated simply returns true.
func (c *Client) GlobalCompleteRegistration(ctx context.Context, a *Auth) (bool, error) {
	activated, needsRegion, msg, err := c.GlobalRegisterStatus(ctx, a)
	if err != nil {
		return false, err
	}
	if activated {
		return true, nil
	}
	if !needsRegion {
		return false, fmt.Errorf("register not activated: %s", msg)
	}
	countries, err := c.GlobalFetchCountries(ctx, a, true)
	if err != nil {
		return false, fmt.Errorf("fetch countries: %w", err)
	}
	if len(countries) == 0 {
		return false, errors.New("no countries available")
	}
	if err := c.GlobalSubmitRegion(ctx, a, countries[0]); err != nil {
		return false, fmt.Errorf("submit region: %w", err)
	}
	activated, _, msg, err = c.GlobalRegisterStatus(ctx, a)
	if err != nil {
		return false, err
	}
	if !activated {
		return false, fmt.Errorf("register still not activated after region submit: %s", msg)
	}
	return true, nil
}
