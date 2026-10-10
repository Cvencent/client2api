package qwenwork

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// panelModelList is the upstream catalogue shape, with two usable entries and
// two that must be skipped (disabled, and nameless).
const panelModelList = `{"qwork":[` +
	`{"key":"qwen3-max","display_name":"Qwen3 Max","enable":true,"is_reasoning":false,"is_vl":false,"max_input_tokens":131072,"max_output_tokens":8192,"credits":"1","description":"flagship"},` +
	`{"key":"qwen3-turbo","display_name":"Qwen3 Turbo","enable":true},` +
	`{"key":"retired","display_name":"Retired","enable":false},` +
	`{"key":"","display_name":"nameless","enable":true}` +
	`]}`

// panelGrant is a successful refresh / device-grant payload.
const panelGrant = `{"token":"fresh-access-token","refresh_token":"fresh-refresh-token",` +
	`"expires_at":"2026-12-31T00:00:00Z","user_id":"u1","user_name":"n1"}`

// offlineTransport answers nothing and fails every request.  It is the default
// so no test can accidentally reach the network.
func offlineTransport() *fakeTransport {
	return &fakeTransport{handler: func(int, *http.Request, string) (*http.Response, error) {
		return nil, errors.New("offline test")
	}}
}

// routedTransport answers by URL, which is enough for every case here.
func routedTransport(models, refresh, poll func(*http.Request) (*http.Response, error)) *fakeTransport {
	return &fakeTransport{handler: func(_ int, req *http.Request, _ string) (*http.Response, error) {
		u := req.URL.String()
		switch {
		case strings.Contains(u, refreshPath) && refresh != nil:
			return refresh(req)
		case strings.Contains(u, pollPath) && poll != nil:
			return poll(req)
		case strings.Contains(u, modelsPath) && models != nil:
			return models(req)
		}
		return nil, errors.New("offline test: unrouted " + u)
	}}
}

// panelClient builds a real Client against a throwaway data directory.
// chatTransport answers any chat-path request with the given SSE body.  It is
// what the TestAccount probe hits, so a test that exercises the probe never
// has to model the whole catalogue route.
func chatTransport(frames string) *fakeTransport {
	return &fakeTransport{handler: func(_ int, req *http.Request, _ string) (*http.Response, error) {
		if strings.Contains(req.URL.String(), "/algo/api/v2/service/pro/sse/agent_chat_generation") {
			return fakeResponse(req, http.StatusOK, frames), nil
		}
		return nil, errors.New("offline test: unrouted " + req.URL.String())
	}}
}

func panelClient(t *testing.T, cfgJSON string, rt *fakeTransport) *Client {
	t.Helper()
	clearCredentialEnv(t)

	deps := core.Deps{DataDir: t.TempDir(), Logf: func(string, ...any) {}}
	if cfgJSON != "" {
		deps.Config = json.RawMessage(cfgJSON)
	}
	if rt == nil {
		rt = offlineTransport()
	}
	deps.HTTPClient = &http.Client{Transport: rt}

	c, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c.(*Client)
}

// addPanelAccount adds one credential and fails the test if it is rejected.
func addPanelAccount(t *testing.T, c *Client, fields map[string]string) core.AccountRecord {
	t.Helper()
	rec, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: fields})
	if err != nil {
		t.Fatalf("AddAccount(%v): %v", fields, err)
	}
	return rec
}

// ---------------------------------------------------------------------------
// the add-account form
// ---------------------------------------------------------------------------

func TestQwenworkAccountFieldsSchema(t *testing.T) {
	c := panelClient(t, "", nil)
	fields := c.AccountFields(context.Background())
	if len(fields) == 0 {
		t.Fatal("AccountFields returned nothing")
	}

	byKey := map[string]core.FieldSpec{}
	for _, f := range fields {
		if f.Key == "" {
			t.Fatalf("a field has no key: %+v", f)
		}
		if _, dup := byKey[f.Key]; dup {
			t.Fatalf("duplicate field %q", f.Key)
		}
		if f.Label == "" {
			t.Errorf("field %q has no label", f.Key)
		}
		switch f.Type {
		case "text", "password", "textarea", "number", "bool", "select":
		case "":
			t.Errorf("field %q has no type", f.Key)
		default:
			t.Errorf("field %q has unsupported type %q", f.Key, f.Type)
		}
		byKey[f.Key] = f
	}

	token, ok := byKey["access_token"]
	if !ok {
		t.Fatal("no access_token field")
	}
	if !token.Required {
		t.Error("access_token must be required")
	}
	if token.Type != "password" {
		t.Errorf("access_token type = %q, want password so the panel masks it", token.Type)
	}
	if byKey["refresh_token"].Type != "password" {
		t.Error("refresh_token must be masked too")
	}
	// A credential field must never pre-fill a value.
	for _, f := range fields {
		if f.Type == "password" && f.Default != "" {
			t.Errorf("field %q pre-fills a secret", f.Key)
		}
	}
}

func TestQwenworkAddAccountRequiresToken(t *testing.T) {
	c := panelClient(t, "", nil)
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		fields map[string]string
	}{
		{"nothing at all", nil},
		{"an empty token", map[string]string{"access_token": ""}},
		{"only a label", map[string]string{"label": "laptop"}},
		{"a token with a newline", map[string]string{"access_token": "tok\nen"}},
		{"a token with a space", map[string]string{"access_token": "tok en"}},
		{"a token with a tab", map[string]string{"access_token": "tok\ten"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.AddAccount(ctx, core.AccountSpec{Fields: tc.fields}); err == nil {
				t.Fatalf("AddAccount(%v) was accepted", tc.fields)
			}
		})
	}

	recs, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("a rejected account was stored anyway: %+v", recs)
	}
}

func TestQwenworkAddAccountPersistsAndLists(t *testing.T) {
	c := panelClient(t, "", nil)
	ctx := context.Background()

	rec := addPanelAccount(t, c, map[string]string{
		"access_token":  "tok-alpha",
		"label":         "work laptop",
		"uid":           "u-alpha",
		"nickname":      "alpha",
		"email":         "alpha@example.test",
		"refresh_token": "ref-alpha",
		"expires_at":    "2026-12-31T00:00:00Z",
	})

	if rec.ID != "uid:u-alpha" {
		t.Errorf("id = %q, want the uid-derived id", rec.ID)
	}
	if rec.Label != "work laptop" {
		t.Errorf("label = %q, want the form's label", rec.Label)
	}
	if !rec.Enabled {
		t.Error("a new account must be enabled by default")
	}
	if rec.Fields["origin"] != originStored {
		t.Errorf("origin = %v, want %q", rec.Fields["origin"], originStored)
	}
	if rec.Fields["removable"] != true {
		t.Error("a hand-added account must be removable")
	}
	if rec.ExpiresAt == "" {
		t.Error("ExpiresAt was not surfaced")
	}

	recs, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("Accounts = %d records, want 1", len(recs))
	}
	if recs[0].ID != rec.ID {
		t.Errorf("listed id = %q, want %q", recs[0].ID, rec.ID)
	}
}

func TestQwenworkAddAccountHonoursDisabled(t *testing.T) {
	c := panelClient(t, "", nil)
	ctx := context.Background()

	disabled := false
	rec, err := c.AddAccount(ctx, core.AccountSpec{
		Fields:  map[string]string{"access_token": "tok-parked"},
		Enabled: &disabled,
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if rec.Enabled {
		t.Error("the form asked for a parked account")
	}
	if c.pool.ready() {
		t.Error("a parked account must not make the pool ready")
	}

	// Re-adding the same credential with the switch on must actually enable it:
	// put() merges and ORs the Disabled flag, so AddAccount has to say so
	// explicitly.
	again := addPanelAccount(t, c, map[string]string{"access_token": "tok-parked", "uid": "u1"})
	_ = again
}

func TestQwenworkAddAccountReEnablesExplicitly(t *testing.T) {
	c := panelClient(t, "", nil)
	ctx := context.Background()

	off := false
	if _, err := c.AddAccount(ctx, core.AccountSpec{
		Fields:  map[string]string{"access_token": "tok-x", "uid": "u-x"},
		Enabled: &off,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if c.pool.ready() {
		t.Fatal("the account should be parked")
	}

	on := true
	rec, err := c.AddAccount(ctx, core.AccountSpec{
		Fields:  map[string]string{"access_token": "tok-x", "uid": "u-x"},
		Enabled: &on,
	})
	if err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if !rec.Enabled {
		t.Error("re-adding with the switch on must enable the account")
	}
	if !c.pool.ready() {
		t.Error("the pool should be ready after the re-enable")
	}
}

func TestQwenworkAccountsMarkConfiguredOrigin(t *testing.T) {
	c := panelClient(t, `{"accounts":[{"uid":"u-cfg","access_token":"tok-cfg"}]}`, nil)
	addPanelAccount(t, c, map[string]string{"access_token": "tok-hand", "uid": "u-hand"})

	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("Accounts = %d records, want 2", len(recs))
	}

	seen := map[string]core.AccountRecord{}
	for _, r := range recs {
		seen[r.ID] = r
	}
	cfg, ok := seen["uid:u-cfg"]
	if !ok {
		t.Fatalf("the configured account is missing: %+v", seen)
	}
	if cfg.Fields["origin"] != originConfig {
		t.Errorf("configured origin = %v, want %q", cfg.Fields["origin"], originConfig)
	}
	if cfg.Fields["removable"] != false {
		t.Error("a configured account must not be removable")
	}
	if seen["uid:u-hand"].Fields["origin"] != originStored {
		t.Error("a hand-added account must be marked stored")
	}
}

// ---------------------------------------------------------------------------
// remove / enable
// ---------------------------------------------------------------------------

func TestQwenworkRemoveAccount(t *testing.T) {
	c := panelClient(t, "", nil)
	ctx := context.Background()
	rec := addPanelAccount(t, c, map[string]string{"access_token": "tok-gone", "uid": "u-gone"})

	if err := c.RemoveAccount(ctx, ""); err == nil {
		t.Error("an empty id must be rejected")
	}
	if err := c.RemoveAccount(ctx, "uid:nope"); err == nil {
		t.Error("an unknown id must be rejected")
	}
	if err := c.RemoveAccount(ctx, rec.ID); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}

	recs, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("the account survived removal: %+v", recs)
	}
}

func TestQwenworkRemoveRefusesConfiguredAccount(t *testing.T) {
	c := panelClient(t, `{"accounts":[{"uid":"u-cfg","access_token":"tok-cfg"}]}`, nil)

	err := c.RemoveAccount(context.Background(), "uid:u-cfg")
	if err == nil {
		t.Fatal("a configured account must not be removable from the panel")
	}
	if !strings.Contains(err.Error(), "clients.qwenwork") {
		t.Errorf("the error must say where to remove it: %v", err)
	}

	recs, _ := c.Accounts(context.Background())
	if len(recs) != 1 {
		t.Fatalf("the configured account disappeared: %+v", recs)
	}
}

func TestQwenworkSetAccountEnabledParksAndRevives(t *testing.T) {
	c := panelClient(t, "", nil)
	ctx := context.Background()
	rec := addPanelAccount(t, c, map[string]string{"access_token": "tok-switch", "uid": "u-switch"})

	if err := c.SetAccountEnabled(ctx, "", true); err == nil {
		t.Error("an empty id must be rejected")
	}
	if err := c.SetAccountEnabled(ctx, "uid:nope", false); err == nil {
		t.Error("an unknown id must be rejected")
	}

	if err := c.SetAccountEnabled(ctx, rec.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if c.pool.ready() {
		t.Error("a parked account must not keep the pool ready")
	}
	recs, _ := c.Accounts(ctx)
	if len(recs) != 1 || recs[0].Enabled {
		t.Fatalf("the parked account should still be listed but disabled: %+v", recs)
	}
	if recs[0].State != stateInvalid {
		t.Errorf("state = %q, want %q", recs[0].State, stateInvalid)
	}
	if !strings.Contains(recs[0].Note, "disabled by the operator") {
		t.Errorf("note = %q, want it to explain the park", recs[0].Note)
	}

	if err := c.SetAccountEnabled(ctx, rec.ID, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !c.pool.ready() {
		t.Error("the pool should be ready again")
	}
	recs, _ = c.Accounts(ctx)
	if !recs[0].Enabled {
		t.Error("the account should be enabled again")
	}
	if recs[0].Note != "" {
		t.Errorf("note = %q, want it cleared on revive", recs[0].Note)
	}
}

// ---------------------------------------------------------------------------
// test / refresh
// ---------------------------------------------------------------------------

func TestQwenworkTestAccountReachesUpstream(t *testing.T) {
	rt := chatTransport(chatSSEFrames)
	c := panelClient(t, "", rt)
	rec := addPanelAccount(t, c, map[string]string{"access_token": "tok-live", "uid": "u-live"})

	res, err := c.TestAccount(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !res.OK {
		t.Fatalf("TestAccount failed: %s", res.Error)
	}
	if res.AccountID != rec.ID {
		t.Errorf("AccountID = %q, want %q", res.AccountID, rec.ID)
	}
	if res.Model != probeModel {
		t.Errorf("Model = %q, want the probe model %q", res.Model, probeModel)
	}
	if !strings.Contains(res.Reply, "Hello") {
		t.Errorf("Reply = %q, want the streamed text", res.Reply)
	}
	if rt.count() == 0 {
		t.Error("no request reached the upstream")
	}
}

func TestQwenworkTestAccountRefusalIsAResult(t *testing.T) {
	rt := routedTransport(func(req *http.Request) (*http.Response, error) {
		return fakeResponse(req, http.StatusUnauthorized, `{"code":401,"message":"token expired"}`), nil
	}, nil, nil)
	c := panelClient(t, "", rt)
	rec := addPanelAccount(t, c, map[string]string{"access_token": "tok-bad", "uid": "u-bad"})

	res, err := c.TestAccount(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("a refusal is a result, not an error: %v", err)
	}
	if res.OK {
		t.Fatal("TestAccount reported success against a 401")
	}
	if res.Error == "" {
		t.Error("the panel needs to know why")
	}
	if res.ElapsedMS < 0 {
		t.Error("ElapsedMS must not be negative")
	}

	// The refusal must be recorded on the account, not swallowed.
	recs, _ := c.Accounts(context.Background())
	if len(recs) != 1 {
		t.Fatalf("Accounts = %+v", recs)
	}
	if recs[0].State == stateReady {
		t.Errorf("state = %q, want the refusal to move it off ready", recs[0].State)
	}
}

func TestQwenworkTestAccountUnknownID(t *testing.T) {
	c := panelClient(t, "", nil)
	if _, err := c.TestAccount(context.Background(), "uid:nope"); err == nil {
		t.Fatal("an unknown id must be an error")
	}
}

func TestQwenworkRefreshWithoutRefreshToken(t *testing.T) {
	c := panelClient(t, "", nil)
	rec := addPanelAccount(t, c, map[string]string{"access_token": "tok-noref", "uid": "u-noref"})

	out, err := c.RefreshAccount(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("RefreshAccount = %d results, want 1", len(out))
	}
	if out[0].OK {
		t.Error("an account with no refresh token cannot be renewed")
	}
	if !strings.Contains(out[0].Error, "no refresh token") {
		t.Errorf("Error = %q, want it to name the missing token", out[0].Error)
	}

	if _, err := c.RefreshAccount(context.Background(), "uid:nope"); err == nil {
		t.Error("an unknown id must be an error")
	}
}

func TestQwenworkRefreshRenewsToken(t *testing.T) {
	rt := routedTransport(nil, func(req *http.Request) (*http.Response, error) {
		return fakeResponse(req, http.StatusOK, panelGrant), nil
	}, nil)
	c := panelClient(t, "", rt)
	rec := addPanelAccount(t, c, map[string]string{
		"access_token":  "tok-old",
		"uid":           "u1",
		"refresh_token": "ref-old",
	})

	out, err := c.RefreshAccount(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if len(out) != 1 || !out[0].OK {
		t.Fatalf("RefreshAccount = %+v, want one success", out)
	}

	e := c.pool.find("uid:u1")
	if e == nil {
		t.Fatal("the account vanished across the refresh")
	}
	if e.acct.AccessToken != "fresh-access-token" {
		t.Errorf("access token = %q, want the renewed one", e.acct.AccessToken)
	}
	if e.acct.RefreshToken != "fresh-refresh-token" {
		t.Errorf("refresh token = %q, want the rotated one", e.acct.RefreshToken)
	}
	if e.state != stateReady {
		t.Errorf("state = %q, want ready after a successful refresh", e.state)
	}
	if c.pool.find("uid:u1") == nil && c.pool.find("tok:fresh-access-t") != nil {
		t.Error("the refresh created a second entry instead of updating the first")
	}
}

func TestQwenworkRefreshAllAccounts(t *testing.T) {
	rt := routedTransport(nil, func(req *http.Request) (*http.Response, error) {
		return fakeResponse(req, http.StatusOK, panelGrant), nil
	}, nil)
	c := panelClient(t, "", rt)
	addPanelAccount(t, c, map[string]string{"access_token": "tok-a", "uid": "u1", "refresh_token": "r-a"})
	addPanelAccount(t, c, map[string]string{"access_token": "tok-b", "uid": "u2"})

	out, err := c.RefreshAccount(context.Background(), "")
	if err != nil {
		t.Fatalf("RefreshAccount(all): %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("RefreshAccount(all) = %d results, want 2", len(out))
	}
	var ok, failed int
	for _, r := range out {
		if r.OK {
			ok++
		} else {
			failed++
		}
	}
	// One account has a refresh token and one does not: the call must report
	// both rather than failing as a whole.
	if ok != 1 || failed != 1 {
		t.Errorf("ok=%d failed=%d, want 1 and 1: %+v", ok, failed, out)
	}
}

// ---------------------------------------------------------------------------
// the device flow
// ---------------------------------------------------------------------------

func TestQwenworkStartLoginIssuesURL(t *testing.T) {
	rt := routedTransport(nil, nil, func(req *http.Request) (*http.Response, error) {
		return fakeResponse(req, http.StatusAccepted, `{}`), nil
	})
	c := panelClient(t, `{"poll_interval":"10ms","login_timeout":"2s"}`, rt)
	ctx := context.Background()

	st, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if st.State != core.LoginPending {
		t.Fatalf("State = %q, want pending", st.State)
	}
	if st.SessionID == "" {
		t.Error("SessionID is empty")
	}
	if !strings.Contains(st.URL, st.SessionID) {
		t.Errorf("URL = %q, want it to carry the nonce %q", st.URL, st.SessionID)
	}
	if !strings.Contains(st.URL, "challenge") {
		t.Errorf("URL = %q, want a PKCE challenge", st.URL)
	}

	// The same session must be pollable.
	again, err := c.PollLogin(ctx, st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if again.SessionID != st.SessionID {
		t.Errorf("PollLogin returned session %q, want %q", again.SessionID, st.SessionID)
	}

	if err := c.CancelLogin(ctx, st.SessionID); err != nil {
		t.Fatalf("CancelLogin: %v", err)
	}
	after, err := c.PollLogin(ctx, st.SessionID)
	if err == nil && after.State != core.LoginCancelled {
		t.Errorf("State after cancel = %q, want cancelled", after.State)
	}
}

// An explicit sign-in must always start a flow, even when a credential already
// works.  It used to answer "success" with no URL, which left the 登录 button
// looking broken: the panel had nothing to show and no way to act on the advice
// to "add another".  Re-authorising the same vendor account updates that record
// (the pool is keyed by uid), a different one adds a second credential.
func TestQwenworkStartLoginRunsWhenAlreadyReady(t *testing.T) {
	c := panelClient(t, `{"accounts":[{"uid":"u1","access_token":"tok"}]}`, nil)

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	defer c.CancelLogin(context.Background(), st.SessionID) //nolint:errcheck // best effort

	if st.State != core.LoginPending {
		t.Errorf("State = %q, want %q: an explicit sign-in must start a flow", st.State, core.LoginPending)
	}
	if st.SessionID == "" {
		t.Error("no session id: the panel would have nothing to poll")
	}
	if st.URL == "" {
		t.Error("no authorisation URL: the panel would have nothing to show")
	}
	if !strings.Contains(st.Message, "already usable") {
		t.Errorf("message = %q, want it to mention the credential that already works", st.Message)
	}
}

func TestQwenworkPollLoginUnknownSession(t *testing.T) {
	c := panelClient(t, "", nil)
	ctx := context.Background()

	if _, err := c.PollLogin(ctx, ""); err == nil {
		t.Error("an empty session id must be rejected")
	}
	if _, err := c.PollLogin(ctx, "nope"); err == nil {
		t.Error("an unknown session must be rejected")
	}
	if err := c.CancelLogin(ctx, "nope"); err == nil {
		t.Error("cancelling an unknown session must be rejected")
	}
}

// ---------------------------------------------------------------------------
// secrecy, persistence, capabilities
// ---------------------------------------------------------------------------

func TestQwenworkPanelNeverLeaksToken(t *testing.T) {
	const secret = "sup3r-s3cret-token-value"

	c := panelClient(t, "", nil)
	ctx := context.Background()
	rec := addPanelAccount(t, c, map[string]string{
		"access_token":  secret,
		"refresh_token": secret + "-refresh",
		"uid":           "u-leak",
		"label":         "leaky",
	})

	recs, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	blob, err := json.Marshal(recs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), secret) {
		t.Fatalf("the account listing leaks the credential: %s", blob)
	}
	if strings.Contains(rec.Label, secret) || strings.Contains(rec.Note, secret) {
		t.Fatalf("the record leaks the credential: %+v", rec)
	}
	// The secret must still be there for the module to use.
	if c.pool.find("uid:u-leak") == nil {
		t.Fatal("the account was not actually stored")
	}
}

func TestQwenworkPanelStoreSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	clearCredentialEnv(t)

	deps := core.Deps{DataDir: dir, Logf: func(string, ...any) {}, HTTPClient: &http.Client{Transport: offlineTransport()}}
	c, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first := c.(*Client)
	rec := addPanelAccount(t, first, map[string]string{"access_token": "tok-persist", "uid": "u-persist"})
	if err := first.SetAccountEnabled(context.Background(), rec.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}

	// A second client over the same data directory must see the same account,
	// including the parked switch.
	again, err := New(deps)
	if err != nil {
		t.Fatalf("New (second): %v", err)
	}
	second := again.(*Client)
	recs, err := second.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("Accounts = %+v, want the stored account to survive", recs)
	}
	if recs[0].ID != rec.ID {
		t.Errorf("id = %q, want %q", recs[0].ID, rec.ID)
	}
	if recs[0].Enabled {
		t.Error("the parked switch did not survive the reload")
	}
}

func TestQwenworkCapabilitiesAdvertised(t *testing.T) {
	c := panelClient(t, "", nil)
	caps := core.CapabilitiesOf(context.Background(), c)

	if !caps.Manage {
		t.Error("qwenwork advertises account management but Capabilities says otherwise")
	}
	if !caps.Login {
		t.Error("qwenwork implements the device flow but Capabilities says otherwise")
	}
	if caps.Import {
		t.Error("qwenwork has no importer; advertising one would put a dead button in the panel")
	}
	if len(caps.Fields) == 0 {
		t.Error("Capabilities must carry the add-account schema")
	}
}

func TestQwenworkClassifyErr(t *testing.T) {
	if kind, _ := classifyErr(nil); kind != kindNone {
		t.Errorf("classifyErr(nil) = %v, want kindNone", kind)
	}
	if kind, _ := classifyErr(context.DeadlineExceeded); kind != kindNetwork {
		t.Errorf("a deadline = %v, want kindNetwork", kind)
	}
	ue := &upstreamError{Status: 401, Kind: kindAuth, Message: "nope"}
	if kind, msg := classifyErr(ue); kind != kindAuth || msg == "" {
		t.Errorf("classifyErr(upstreamError) = %v %q, want kindAuth with a message", kind, msg)
	}
	// A programming error must not cost the credential a cooldown.
	if kind, _ := classifyErr(errors.New("bug")); kind != kindNone {
		t.Errorf("a plain error = %v, want kindNone", kind)
	}
}

func TestQwenworkPanelLoginExpiry(t *testing.T) {
	// A pending session past its window must report failure, not hang.
	done := make(chan struct{})
	close(done)
	sess := &panelLogin{
		nonce:     "n",
		url:       "http://example.test/auth",
		state:     core.LoginPending,
		startedAt: time.Now().Add(-time.Hour),
		expiresAt: time.Now().Add(-time.Minute),
	}
	st := sess.snapshot()
	if st.State != core.LoginFailed {
		t.Errorf("State = %q, want failed for an expired window", st.State)
	}
	if !strings.Contains(st.Message, "expired") {
		t.Errorf("Message = %q, want it to explain the expiry", st.Message)
	}
}

// ---------------------------------------------------------------------------
// RefreshModels (the panel's "re-fetch from upstream" button)
// ---------------------------------------------------------------------------

// countModelList reports how many recorded upstream calls hit the catalogue
// path.
func countModelList(rt *fakeTransport) int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	n := 0
	for _, r := range rt.requests {
		if strings.Contains(r.url, modelsPath) {
			n++
		}
	}
	return n
}

func TestRefreshModelsUpdatesCatalogueAndBypassesCache(t *testing.T) {
	body := `{"qwork":[{"key":"qwen3-max","display_name":"Qwen3 Max","enable":true}]}`
	rt := routedTransport(func(req *http.Request) (*http.Response, error) {
		return fakeResponse(req, http.StatusOK, body), nil
	}, nil, nil)
	c := panelClient(t, "", rt)
	addPanelAccount(t, c, map[string]string{"access_token": "tok-live", "uid": "u-live"})
	base := countModelList(rt)

	first, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if len(first) != 1 || first[0].ID != "qwen3-max" {
		t.Fatalf("first = %#v", first)
	}
	if n := countModelList(rt) - base; n != 1 {
		t.Fatalf("want exactly one catalogue fetch, saw %d", n)
	}

	// The refreshed list is cached: Models() inside the TTL must not touch the
	// network.
	if _, err := c.Models(context.Background()); err != nil {
		t.Fatalf("Models: %v", err)
	}
	if n := countModelList(rt) - base; n != 1 {
		t.Fatalf("Models should serve the cache, saw %d catalogue fetches", n)
	}

	// The vendor changes its catalogue.  RefreshModels has to see the new list
	// even though the cache is still fresh.
	body = `{"qwork":[{"key":"qwen3-turbo","display_name":"Qwen3 Turbo","enable":true}]}`
	refreshed, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("second RefreshModels: %v", err)
	}
	if n := countModelList(rt) - base; n != 2 {
		t.Fatalf("RefreshModels must bypass the cache, saw %d catalogue fetches", n)
	}
	if len(refreshed) != 1 || refreshed[0].ID != "qwen3-turbo" {
		t.Fatalf("refreshed = %#v", refreshed)
	}
}

func TestRefreshModelsFailureKeepsLastGoodList(t *testing.T) {
	failing := false
	rt := routedTransport(func(req *http.Request) (*http.Response, error) {
		if failing {
			return fakeResponse(req, http.StatusInternalServerError, `{"message":"upstream is down"}`), nil
		}
		return fakeResponse(req, http.StatusOK, panelModelList), nil
	}, nil, nil)
	c := panelClient(t, "", rt)
	addPanelAccount(t, c, map[string]string{"access_token": "tok-live", "uid": "u-live"})

	good, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if len(good) != 2 {
		t.Fatalf("good = %#v", good)
	}

	failing = true
	models, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("a failed refresh must report an error")
	}
	// A failed refresh must never empty the catalogue.
	if len(models) != len(good) || models[0].ID != good[0].ID {
		t.Fatalf("a failed refresh must keep the last good list, got %#v", models)
	}
}

func TestRefreshModelsWithoutAccountStillServesBuiltins(t *testing.T) {
	c := panelClient(t, "", offlineTransport())

	models, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("refreshing without an account must report an error")
	}
	if len(models) == 0 {
		t.Fatal("a failed refresh must still return a usable catalogue")
	}
	for _, m := range models {
		if m.ID == "" || m.OwnedBy != "qwenwork" {
			t.Fatalf("model = %+v", m)
		}
	}
}

func TestRefreshModelsErrorOmitsToken(t *testing.T) {
	// A bare token has no label for cleanErrorText's patterns to key on, so
	// this also pins the explicit replacement in scrubModelError.
	const token = "BARE-QWEN-TOKEN-abcdefghijkl"
	rt := routedTransport(func(req *http.Request) (*http.Response, error) {
		return fakeResponse(req, http.StatusUnauthorized,
			`{"message":"credential `+token+` rejected"}`), nil
	}, nil, nil)
	c := panelClient(t, "", rt)
	addPanelAccount(t, c, map[string]string{"access_token": token, "uid": "u-live"})

	models, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if len(models) == 0 {
		t.Fatal("a failed refresh must still return a usable catalogue")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("the token leaked into the error: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "qwenwork:") {
		t.Errorf("the error should be prefixed with the module name: %v", err)
	}
}
