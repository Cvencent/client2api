# Client2API Free Login Design

Date: 2026-10-02

Status: Approved in conversation on 2026-10-02; implemented. See the task list in
`docs/superpowers/plans/2026-10-02-client2api-free-login.md`.

## Goal

Add interactive account login to the `opencode`, `openrouter`, and `raccoon`
modules so the panel's Add Account flow can obtain credentials without asking
the operator to copy a token by hand.

The feature must preserve the existing `core.LoginProvider` and
`core.RealmLoginProvider` contracts. The panel and core interfaces must not
need a new login protocol.

The user's target is to use each provider's free models or free allowance:

- OpenCode: anonymous `space-bunny-free` access and OpenCode Console OAuth.
- OpenRouter: OAuth-issued API keys and free-model metadata.
- Raccoon: WeChat QR login, SMS login, refreshable credentials, and the
  existing desktop login-points reward.

## Non-Goals

- No new panel UI beyond what the existing login capability already renders.
- No new global credential store.
- No browser automation, CDP, or external QR image service.
- No attempt to claim Raccoon's daily 300 credits through an endpoint; the
  vendor grants those server-side and exposes no claim API.
- No filtering of OpenRouter or Raccoon accounts to free models only. Free
  models are identified and preferred for probes, but the account's paid quota
  remains usable.

## Architecture

Each provider implements `core.LoginProvider` in its own package. OpenCode
implements `core.RealmLoginProvider` so the existing realm picker can offer
two login choices without changing the panel.

Login sessions live in memory in the provider process. A session owns:

- a random session id;
- a `pending|success|failed|cancelled` state;
- a start time and a bounded timeout;
- any loopback listener or device-flow state it needs;
- a terminal-state lock so late callbacks cannot overwrite success or
  cancellation.

Successful login writes through the provider's existing panel-owned account
store. Remove, enable/disable, refresh, test, and pool selection keep working
without special cases.

Secrets never enter `core.AccountRecord`, panel JSON, URLs returned to the
panel, or error messages. Panel records may contain only masked values and
non-secret metadata.

## OpenCode

### Login Realms

`LoginRealms` returns two entries:

- `free`: anonymous OpenCode Zen access.
- `oauth`: OpenCode Console account login.

`StartLogin` uses the module's configured default realm. The default is
`free`, because it is the no-credential path the user asked for.

### Anonymous Free Mode

`StartLoginRealm(ctx, "free")` creates or reuses a panel-owned account with:

- `auth_mode = anonymous`
- API credential literal `public`
- request header `x-api-key: public`
- no `Authorization` header

The account is immediately ready. It must not be sent to OpenCode's model
list as if it were a paid key.

The anonymous model allowlist is conservative. The only built-in model that
was live-tested successfully is:

- `space-bunny-free`

Other free-looking ids returned `FreeTierError`, a region error, or an
unavailable-model response during testing. The config may add ids through a
dedicated allowlist field, but the module must not silently enable every id
whose name ends in `-free`.

Requests for a model outside the allowlist fail locally with a clear message.
They must not make an upstream request.

### OpenCode Console OAuth

`StartLoginRealm(ctx, "oauth")` starts the device flow:

1. `POST https://opencode.ai/console/auth/device/code` with
   `{"client_id":"opencode-cli"}`.
2. Return `verification_uri_complete` as `LoginState.URL` and `user_code` as
   `LoginState.Code`.
3. Poll `POST https://opencode.ai/console/auth/device/token` with:
   - `grant_type=urn:ietf:params:oauth:grant-type:device_code`
   - `device_code`
   - `client_id=opencode-cli`
4. Treat `authorization_pending` as pending, add five seconds after
   `slow_down`, and fail on `expired_token`, `access_denied`, or an unknown
   terminal error.
5. On success, fetch `GET /console/api/user` and `GET /console/api/orgs`.
6. Select the first organization deterministically, sorted by name and id.
   Store its id and name with the credential.
7. Persist access token, refresh token, expiry, account id, email, org id, and
   org name.

OAuth inference requests use:

- `Authorization: Bearer <access_token>`
- `x-opencode-org-id: <org_id>` when an org id is present.

Static API keys keep the existing Bearer-key behavior. The credential type,
not the string shape, decides the headers.

OAuth refresh uses the same token endpoint with:

- `grant_type=refresh_token`
- `refresh_token`
- `client_id=opencode-cli`

Refresh proactively five minutes before expiry. An authentication rejection
marks the account invalid; a transient transport error does not delete the
credential or corrupt its state.

The OAuth account's model list comes from the workspace config endpoint,
filtered to enabled and whitelisted models. Zero-cost entries are marked as
free metadata. The account may use its full allowed catalogue because its free
allowance or credits are settled by the vendor.

## OpenRouter

### OAuth Login

`StartLogin` starts a one-shot loopback callback server:

- bind `127.0.0.1` on an ephemeral port;
- create a PKCE verifier and S256 challenge;
- create a random `state`;
- build the authorization URL:

  `https://openrouter.ai/auth?callback_url=<loopback>&code_challenge=<S256>&code_challenge_method=S256&key_label=<label>`

Return that URL in `LoginState.URL`.

The callback handler validates `state`, rejects missing or duplicate codes,
exchanges the authorization code at:

`POST https://openrouter.ai/api/v1/auth/keys`

with:

```json
{
  "code": "<authorization code>",
  "code_verifier": "<PKCE verifier>",
  "code_challenge_method": "S256"
}
```

The response key is persisted through the existing OpenRouter credential
store as a normal API key. The key fingerprint remains the account id, so
logging in twice with the same key updates one account.

OpenRouter keys do not expire and have no refresh token. `RefreshAccount`
continues to validate the key against `GET /key`.

### Free Models

When parsing the public model catalogue, mark a model as free when both
`pricing.prompt` and `pricing.completion` parse to zero. Missing or
unparseable pricing is not free.

The configured `test_model` always wins. Otherwise the account test prefers
the first free model from the current catalogue; if no free model is known,
it keeps the existing fallback test model.

## Raccoon

### Login Page

`StartLogin` starts a loopback server on `127.0.0.1` with an ephemeral port
and returns:

`http://127.0.0.1:<port>/raccoon/login`

The server sets an `HttpOnly`, `SameSite=Strict` session cookie scoped to the
local `/raccoon` paths. The local API endpoints require that cookie and reject
cross-origin requests. The page URL does not carry a reusable session token.

The page has two tabs:

- WeChat QR login.
- SMS login.

The page receives only rendered HTML/JSON needed for display: it does not
receive access tokens, refresh tokens, the phone cipher secret, or a
separate host-side QR session value.

### QR Login

The host generates a 32-hex-character QR code locally. The QR encodes:

`https://xiaohuanxiong.com/login/mp?code=<code>&appname=商汤小浣熊官网`

The QR image is generated in-process as SVG. No external QR image service is
used. The module adds a small dependency-free QR encoder for byte mode,
error-correction level M, and versions 1 through 10.

The page polls the local endpoint every two seconds. The host polls the
vendor at:

`POST /api/web/auth/v1/login_with_qrcode_code`

with `{"qrcode_code":"<code>"}`.

State mapping:

- `pending`: keep waiting.
- `logging`: show that the code was scanned.
- `canceled`: generate a new code, render a new QR, and keep waiting.
- `success`: require a non-empty access token; persist the credential.
- malformed, unknown, or temporary network failure: keep waiting until the
  login timeout.

### SMS Login

The SMS tab dynamically loads the vendor's Aliyun captcha SDK only after the
operator selects that tab.

Send-code request:

`POST /api/web/auth/v1/send_sms`

Body:

```json
{
  "captcha_param": "<captcha token>",
  "nation_code": "86",
  "phone": "<AES-128-CFB encrypted phone>"
}
```

Login request:

`POST /api/web/auth/v1/login_with_sms`

Body:

```json
{
  "nation_code": "86",
  "phone": "<AES-128-CFB encrypted phone>",
  "sms_code": "<code>"
}
```

The phone cipher and key stay host-side. The page sends only the plain phone
number and SMS code to the local server. Captcha, SMS, and phone errors are
returned to the page and remain retryable.

### Credential Persistence

A successful Raccoon login reuses the existing credential structure:
`access_token`, `refresh_token`, `expires_at`, `office_identity`, `user_id`,
`nickname`, `phone`, and `device_id`.

After login, best-effort enrich the credential through
`GET /api/web/auth/v1/user_info`. Enrichment failure does not fail the login.

Store the credential through the existing removable account path. The account
id must use the existing identity rule so logging in again updates the same
account instead of creating a duplicate.

After storing the credential, best-effort call the existing
`login-points` reward action. A refusal or failure does not fail the login.
The daily 300 credits remain untouched because no endpoint exists.

Refresh continues through the existing `POST /api/web/auth/v1/refresh` path.

## Error Handling

- Login sessions are single-use and terminal states are sticky.
- Cancel is idempotent.
- Polling a finished session returns the same terminal state.
- Login timeouts release loopback listeners and close pending sessions.
- OpenCode device-flow transport failures remain pending until expiry; an
  explicit terminal response fails the session.
- OpenRouter callback state mismatch, missing code, exchange failure, or
  timeout fails the session.
- Raccoon QR temporary errors stay pending; a canceled QR refreshes the code;
  a success response without an access token fails the session.
- Raccoon SMS/captcha errors are shown on the local page and allow retry.
- Secret-bearing values are scrubbed from logs and error strings.

## Security

- Loopback servers bind only `127.0.0.1` and use ephemeral ports.
- OpenRouter uses PKCE S256 and validates `state`.
- Raccoon local endpoints require a random `HttpOnly` `SameSite=Strict`
  session cookie and reject cross-origin requests.
- Local request bodies are size-limited and methods are checked.
- No CORS headers are emitted.
- No third-party QR renderer or QR image host is used.
- The Aliyun captcha script is loaded only for SMS login.
- Access tokens, refresh tokens, authorization codes, device codes, and full
  phone numbers never appear in panel records or logs.
- Existing credential files remain the only persistence mechanism; no new
  global secret store is introduced.

## Testing

Implementation follows RED, GREEN, REFACTOR. Tests use `httptest` fake
upstreams and do not require live vendor accounts.

### OpenCode

- Device-code start, pending, slow-down, success, denial, and expiry.
- Refresh success, authentication rejection, and transient failure.
- Header selection for anonymous, OAuth, and static API-key credentials.
- Anonymous model allowlist and local rejection of unsupported models.
- OAuth identity stability across token refresh.
- Credential persistence and reload without leaking secrets into panel rows.

### OpenRouter

- PKCE verifier/challenge vector.
- Authorization URL construction.
- State mismatch, missing code, duplicate callback, exchange failure, and
  timeout.
- Key persistence, duplicate-key update, and panel masking.
- Free-model detection from pricing fields.
- Test-model selection with and without a free model.

### Raccoon

- QR URL construction and QR SVG golden vectors.
- Pending, logging, canceled, success, and malformed QR responses.
- Canceled QR code rotation.
- SMS phone encryption, captcha parameter forwarding, and SMS login.
- Local server cookie enforcement and cross-origin rejection.
- Credential enrichment and login-points failure not failing login.
- Refresh and account identity deduplication.

### Verification

Run focused unit tests for the three packages, then the race detector for
those packages. Finish with a live smoke test through the running panel:

1. Add each account through its login flow.
2. Confirm the account appears once.
3. Run the panel's account test.
4. Send one request through each provider using an available free model.

Update each module's README and the capability matrix when the implementation
lands.

## Acceptance Criteria

- The panel's Add Account flow offers a working login action for all three
  providers.
- OpenCode can create an anonymous free account and complete Console OAuth.
- OpenRouter can complete OAuth through a local callback and store the issued
  key.
- Raccoon can complete QR and SMS login and store a refreshable credential.
- Free models are clearly identified where the provider exposes pricing.
- Existing manual credential entry, account removal, enable/disable, refresh,
  and account testing keep working.
- No secret appears in panel output, URLs, or logs.
