# Client2API Free Login Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Add interactive login to `opencode`, `openrouter`, and `raccoon`, with free-model/free-allowance behavior.

**Architecture:** Keep `core.LoginProvider` and `core.RealmLoginProvider` unchanged. Each provider owns its login session, refresh logic, and credential persistence; successful logins write through the existing account pools.

**Tech Stack:** Go 1.27 standard library, existing `client2api/internal/core`, `httptest`, existing provider pools.

---

## Task 1: OpenCode credential kinds and request headers

**Files:**
- Modify: `clients/opencode/pool.go`
- Modify: `clients/opencode/opencode.go`
- Modify: `clients/opencode/accounts.go`
- Modify: `clients/opencode/config.go`
- Test: `clients/opencode/accounts_test.go`
- Test: `clients/opencode/opencode_test.go`

- [x] **Step 1: Write failing tests**

Add tests that construct an `accountRecord` with `AuthMode: "anonymous"` and assert `do` sends `x-api-key: public` and no `Authorization`; construct an OAuth record with `AccessToken`, `OrgID`, and assert `Authorization: Bearer <token>` plus `x-opencode-org-id`.

- [x] **Step 2: Run tests to verify failure**

Run: `go test ./clients/opencode -run 'Test.*AuthMode|Test.*OAuth' -count=1`
Expected: FAIL because `AuthMode` and OAuth fields do not exist.

- [x] **Step 3: Implement minimal credential model**

Add to `accountRecord`:

```go
AuthMode    string   `json:"auth_mode,omitempty"` // api_key|anonymous|oauth
AccessToken string   `json:"access_token,omitempty"`
RefreshToken string  `json:"refresh_token,omitempty"`
ExpiresAt   string   `json:"expires_at,omitempty"`
OrgID       string   `json:"org_id,omitempty"`
OrgName     string   `json:"org_name,omitempty"`
Email       string   `json:"email,omitempty"`
AllowedModels []string `json:"allowed_models,omitempty"`
```

Add helpers:

```go
func (a *accountRecord) credentialPresent() bool {
    return strings.TrimSpace(a.APIKey) != "" || strings.TrimSpace(a.AccessToken) != ""
}
func (a *accountRecord) authMode() string {
    if a.AuthMode != "" { return a.AuthMode }
    return "api_key"
}
```

Update pool admission/state/upsert to use `credentialPresent()`.

- [x] **Step 4: Implement request header selection**

Add fields to `requestSpec`:

```go
apiKey string
orgID  string
```

In `do`, set `x-api-key` when `apiKey != ""`; otherwise set Bearer when `bearer != ""`; set `x-opencode-org-id` when `orgID != ""`. Update `openChat` and `TestAccount` to build request specs from the account kind.

- [x] **Step 5: Run tests**

Run: `go test ./clients/opencode -count=1`
Expected: PASS.

---

## Task 2: OpenCode anonymous free realm

**Files:**
- Create: `clients/opencode/login.go`
- Modify: `clients/opencode/opencode.go`
- Modify: `clients/opencode/config.go`
- Test: `clients/opencode/login_test.go`

- [x] **Step 1: Write failing test**

```go
func TestAnonymousLoginCreatesPublicAccount(t *testing.T) {
    c := newLoginTestClient(t)
    st, err := c.StartLoginRealm(context.Background(), "free")
    if err != nil { t.Fatal(err) }
    if st.State != core.LoginSuccess || st.AccountID == "" { t.Fatalf("state=%+v", st) }
    acct, ok := c.pool.byID(st.AccountID)
    if !ok || acct.AuthMode != "anonymous" || acct.APIKey != "public" { t.Fatalf("account=%+v", acct) }
}
```

- [x] **Step 2: Run test to verify failure**

Run: `go test ./clients/opencode -run TestAnonymousLoginCreatesPublicAccount -count=1`
Expected: FAIL because `StartLoginRealm` is undefined.

- [x] **Step 3: Implement realm contract**

Create `login.go` with:

```go
func (c *Client) LoginRealms(context.Context) []core.LoginRealm {
    return []core.LoginRealm{
        {Code: "free", Name: "免费模式", Help: "Use OpenCode Zen's anonymous free model."},
        {Code: "oauth", Name: "OpenCode Console", Help: "Sign in with the OpenCode device-code flow."},
    }
}
func (c *Client) StartLogin(ctx context.Context) (core.LoginState, error) {
    return c.StartLoginRealm(ctx, c.cfg.defaultRealm())
}
```

For `free`, upsert an account with stable id `opencode:anonymous`, `Label: "OpenCode Free"`, `AuthMode: "anonymous"`, `APIKey: "public"`, `Source: sourcePanel`, persist, and return success.

- [x] **Step 4: Run test**

Run: `go test ./clients/opencode -run TestAnonymousLoginCreatesPublicAccount -count=1`
Expected: PASS.

---

## Task 3: OpenCode device-code OAuth and refresh

**Files:**
- Modify: `clients/opencode/login.go`
- Modify: `clients/opencode/opencode.go`
- Modify: `clients/opencode/config.go`
- Test: `clients/opencode/login_test.go`

- [x] **Step 1: Write failing tests**

Use `httptest` to serve `/auth/device/code`, `/auth/device/token`, `/api/user`, `/api/orgs`. Assert:

- start returns `verification_uri_complete` and `user_code`;
- pending then success produces an OAuth account with `AuthMode: "oauth"`;
- refresh updates `AccessToken` without changing the account id.

- [x] **Step 2: Run tests to verify failure**

Run: `go test ./clients/opencode -run 'TestOAuth' -count=1`
Expected: FAIL.

- [x] **Step 3: Implement device flow**

Add config fields `AuthBaseURL string` and `DefaultRealm string`. Normalize `AuthBaseURL` to `https://opencode.ai/console`. Implement a mutex-protected `loginSessions map[string]*loginSession`. Poll with the server interval, handle `authorization_pending`, `slow_down`, `expired_token`, and `access_denied`.

After the user and orgs calls, fetch `GET /api/config` with the access token and `x-org-id`; store the enabled, whitelisted model ids in `AllowedModels`. A config fetch failure must not fail the login; the account is still usable and the model list falls back to the public catalogue.

- [x] **Step 4: Implement OAuth refresh**

Add `ensureFreshOAuth` in `login.go`; call it before OpenCode chat and TestAccount. Refresh five minutes before expiry through the token endpoint. Persist the new access/refresh/expiry and preserve the account id.

- [x] **Step 5: Run tests**

Run: `go test ./clients/opencode -count=1`
Expected: PASS.

---

## Task 4: OpenCode free model filtering and metadata

**Files:**
- Modify: `clients/opencode/config.go`
- Modify: `clients/opencode/models.go`
- Modify: `clients/opencode/opencode.go`
- Test: `clients/opencode/models_test.go`

- [x] **Step 1: Write failing tests**

Assert `space-bunny-free` is marked `free: true`, an anonymous-only pool lists only allowed free models, and a chat request for a non-allowed model fails locally without an HTTP request.

- [x] **Step 2: Run tests to verify failure**

Run: `go test ./clients/opencode -run 'Test.*Free|Test.*Anonymous' -count=1`
Expected: FAIL.

- [x] **Step 3: Implement allowlist**

Add `FreeModels []string` to config. Built-in allowlist is `space-bunny-free`. Add `anonymousModelAllowed` and `poolHasOnlyAnonymous`. In `Models`, filter to allowed ids when every account is anonymous; otherwise annotate allowed models with `Extra["free"] = true`.

For OAuth accounts, union their `AllowedModels` into the served catalogue when non-empty; keep the public catalogue when every OAuth account lacks a workspace list. Mark zero-cost entries as free metadata where the response exposes it.

- [x] **Step 4: Run tests**

Run: `go test ./clients/opencode -count=1`
Expected: PASS.

---

## Task 5: OpenRouter OAuth login

**Files:**
- Create: `clients/openrouter/login.go`
- Modify: `clients/openrouter/openrouter.go`
- Modify: `clients/openrouter/config.go`
- Test: `clients/openrouter/login_test.go`

- [x] **Step 1: Write failing tests**

Assert the authorization URL contains `callback_url`, `code_challenge`, and `code_challenge_method=S256`; a callback with the wrong `state` fails; a correct callback exchanges the code and stores the returned key.

- [x] **Step 2: Run tests to verify failure**

Run: `go test ./clients/openrouter -run 'Test.*Login|Test.*PKCE' -count=1`
Expected: FAIL.

- [x] **Step 3: Implement login session**

Add `AuthURL` and `LoginTimeout` config. Start a `127.0.0.1` ephemeral listener, create a 32-byte PKCE verifier and SHA-256 challenge, store state, and return the authorization URL. The callback validates state, exchanges at `POST /api/v1/auth/keys`, and writes the key through `pool.upsert` + `persistCredentials` + `persistState`.

- [x] **Step 4: Implement polling and cancellation**

`PollLogin` reads the callback channel, returns success/failed/cancelled, and treats a timeout as failed. `CancelLogin` shuts down the listener idempotently.

- [x] **Step 5: Run tests**

Run: `go test ./clients/openrouter -count=1`
Expected: PASS.

---

## Task 6: OpenRouter free-model metadata and probe selection

**Files:**
- Modify: `clients/openrouter/models.go`
- Modify: `clients/openrouter/accounts.go`
- Modify: `clients/openrouter/config.go`
- Test: `clients/openrouter/models_test.go`
- Test: `clients/openrouter/accounts_test.go`

- [x] **Step 1: Write failing tests**

Assert a model with zero prompt/completion pricing gets `Extra["free"] = true`, and `TestAccount` prefers the first cached free model when `test_model` is empty.

- [x] **Step 2: Run tests to verify failure**

Run: `go test ./clients/openrouter -run 'Test.*Free|Test.*Probe' -count=1`
Expected: FAIL.

- [x] **Step 3: Implement free detection**

Parse pricing strings with `strconv.ParseFloat`; mark free only when both values are zero. Add `probeModel()` that uses configured `TestModel`, then the first cached free model, then the existing fallback.

- [x] **Step 4: Run tests**

Run: `go test ./clients/openrouter -count=1`
Expected: PASS.

---

## Task 7: Raccoon dependency-free QR encoder

**Files:**
- Create: `clients/raccoon/qr.go`
- Test: `clients/raccoon/qr_test.go`

- [x] **Step 1: Write failing tests**

Use fixed QR vectors for byte mode, ECC level M, versions 1 and 7. Assert matrix size and known dark/light module coordinates. Assert an over-capacity string returns an error instead of a malformed matrix.

- [x] **Step 2: Run tests to verify failure**

Run: `go test ./clients/raccoon -run TestQR -count=1`
Expected: FAIL because `qr.go` does not exist.

- [x] **Step 3: Implement minimal encoder**

Implement byte-mode QR encoding, ECC level M, versions 1-10, Reed-Solomon error correction, mask scoring, format/version bits, and SVG rendering. No third-party imports.

- [x] **Step 4: Run tests**

Run: `go test ./clients/raccoon -run TestQR -count=1`
Expected: PASS.

---

## Task 8: Raccoon login session and QR page

**Files:**
- Create: `clients/raccoon/login.go`
- Create: `clients/raccoon/login_page.go`
- Modify: `clients/raccoon/raccoon.go`
- Modify: `clients/raccoon/config.go`
- Test: `clients/raccoon/login_test.go`

- [x] **Step 1: Write failing tests**

Assert `StartLogin` returns a `127.0.0.1` URL, the login page contains a QR SVG and both tabs, QR polling maps `pending`/`logging`/`canceled`/`success`, and a canceled QR rotates the code.

- [x] **Step 2: Run tests to verify failure**

Run: `go test ./clients/raccoon -run TestLogin -count=1`
Expected: FAIL.

- [x] **Step 3: Implement session and page**

Add `LoginProvider` with `StartLogin`, `PollLogin`, and `CancelLogin`. Bind loopback only, set an `HttpOnly` `SameSite=Strict` cookie, and serve `/raccoon/login`, `/raccoon/poll`, `/raccoon/sms/send`, and `/raccoon/sms/verify`. Generate the QR locally with `renderQRSVG`.

- [x] **Step 4: Implement QR polling**

Call `POST /api/web/auth/v1/login_with_qrcode_code`. On success require an access token, enrich through `user_info`, persist through `pool.put`, and best-effort call the existing `Checkin` login-points action.

- [x] **Step 5: Run tests**

Run: `go test ./clients/raccoon -run TestLogin -count=1`
Expected: PASS.

---

## Task 9: Raccoon SMS login

**Files:**
- Modify: `clients/raccoon/login_page.go`
- Modify: `clients/raccoon/login.go`
- Modify: `clients/raccoon/api.go`
- Test: `clients/raccoon/login_test.go`

- [x] **Step 1: Write failing tests**

Assert the local SMS endpoint rejects missing captcha/phone, sends the encrypted phone to `send_sms`, and logs in through `login_with_sms`. Assert SMS errors are returned to the page without terminating the session.

- [x] **Step 2: Run tests to verify failure**

Run: `go test ./clients/raccoon -run TestSMS -count=1`
Expected: FAIL.

- [x] **Step 3: Implement SMS endpoints and page script**

Load the Aliyun captcha script only when the SMS tab is selected. Post plain phone/captcha to the local server, encrypt phone host-side with `encryptPhoneRandom`, and keep the session pending on retryable SMS errors.

- [x] **Step 4: Run tests**

Run: `go test ./clients/raccoon -count=1`
Expected: PASS.

---

## Task 10: Documentation and full verification

**Files:**
- Modify: `clients/opencode/README.md`
- Modify: `clients/openrouter/README.md`
- Modify: `clients/raccoon/README.md`
- Modify: `docs/MODULE-CONTRACT.md`
- Modify: `README.md`

- [x] **Step 1: Update capability tables and module docs**

Document the new login capabilities, free-model behavior, security boundaries, and the absence of a Raccoon daily-300 endpoint.

- [x] **Step 2: Run focused tests**

Run: `go test ./clients/opencode ./clients/openrouter ./clients/raccoon -count=1`
Expected: PASS.

- [ ] **Step 3: Run race tests** — blocked in this environment: `-race`
  requires cgo and no `gcc`/`clang`/`cl` is installed. Re-run on a host with a
  C toolchain.

Run: `go test -race ./clients/opencode ./clients/openrouter ./clients/raccoon -count=1`
Expected: PASS.

- [x] **Step 4: Run the full suite**

Run: `go test ./... -count=1`
Expected: PASS.

- [x] **Step 5: Live smoke test**

Done against the restarted gateway (`127.0.0.1:8788`, build 2026-10-02 16:10):

- OpenCode `free`: `POST /login {"realm":"free"}` → `success`, account
  `opencode:anonymous` ready; the Test button returned
  `ok:true model:"space-bunny-free" reply:"pong"`; and
  `POST /v1/chat/completions {"model":"opencode/space-bunny-free"}` returned
  `200` with `content:"pong"`. (A first probe with the default `gpt-5-nano`
  answered `401 AuthError: Missing API key.` — that is the paid model being
  refused, and it drove the anonymous probe-model fix.)
- OpenCode `oauth`: `POST /login {"realm":"oauth"}` returned `pending` with a
  real user code and an absolute
  `https://opencode.ai/console/device?user_code=…` URL. The live vendor returns
  a *relative* path, which the module did not resolve before this smoke test;
  `Config.resolveAuthURL` now fixes that. The device authorisation itself was
  **not** completed — that needs the operator.
- OpenRouter: `POST /login` returned a well-formed PKCE URL (`callback_url`,
  `code_challenge`, `code_challenge_method=S256`, `key_label`, `state`); a
  callback with the wrong `state` was rejected `400` and the session went
  `failed`; the catalogue marks the two free ids `cohere/north-mini-code:free`
  and `openrouter/free` as `free:true`. A real browser authorization was
  **not** completed — that needs the operator.
- Raccoon: `POST /login` returned a `127.0.0.1` page URL; the page carried the
  QR SVG and both tabs, sent `Cache-Control: no-store`, and leaked no token or
  `qrcode_code`; the SMS endpoints rejected a missing phone (`400`) and a
  missing code (`400`) and rejected `GET` (`405`). A real WeChat scan or SMS
  was **not** completed — that needs the operator.
