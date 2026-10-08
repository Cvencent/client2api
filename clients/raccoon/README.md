# `raccoon` client

Talks to **商汤小浣熊 / SenseTime Raccoon Work** (`https://xiaohuanxiong.com`), the
vendor's own chat product. The inference endpoint is OpenAI-compatible
(`POST /api/web/llm/v2/chat/completions`, standard SSE), so the module is a thin
authenticated adapter around it plus a credential pool, a model catalogue and a
read-only credits view.

- Route prefix: `raccoon/<model>`, e.g. `raccoon/sn-kimi-k3` (or `raccoon/Kimi-K3 · x1`).
- Upstream wire protocol: OpenAI `chat.completion.chunk` over SSE, plus a
  `{code, message, details, data}` envelope on the non-streaming endpoints.
- Auth: a raccoon JWT (`access_token`) carried as `Authorization: Bearer …`,
  with an optional `refresh_token` for renewal and an `X-Org-Code` header that
  is empty for a personal account and the org code otherwise.
- Sign-in: an interactive loopback page with WeChat **QR** and **SMS** tabs
  (`core.LoginProvider`), plus credential import for an existing token.
- Dependencies: **standard library only**. AES-128-CFB, base64 and JWT payload
  decoding are all `crypto/*` / `encoding/*` and hand-written parsing.

This module was written clean-room from a reference TypeScript implementation
(`iJetLi/deepseek-harness-codearts`, files `src/raccoon*.ts` and
`docs/adding-a-new-provider.md`). It shares no code with it.

---

## What this module deliberately does NOT do

**No third-party dependencies.** The interactive sign-in page is served from
the standard library only — including a hand-written QR encoder (`qr.go`) — so
`go.mod` is unchanged.

**No check-in for the daily 300 credits.** Raccoon's daily 300 credits are
granted by the server (`daily_grant` in the points ledger) and there is **no
endpoint** for them. The reference warns explicitly that this must not become a
check-in button, because the button could only ever fail.

What *is* exposed is the **desktop login grant** —
`POST /api/web/desktop/v1/login/points/grant`, action id `login-points`. The
vendor's own client fires it from a React hook on every launch and shows a popup
only when the reply says `granted:true`. Note what this module does **not**
claim: the reply is a bare `{granted, popup?}` with no reason field, and
**nothing in the client documents the window the server enforces** — so the
button is not described as daily, weekly or one-off. It asks, and reports
`granted:false` as a refusal. The 300 daily credits remain unclaimed by
anything here.

**No vendor-storage auto-discovery.** See "Where credentials come from".

**Nothing is fabricated.** Credits are reported only when
`GET /api/web/points/v1/balance` really returns `available_points`; otherwise
`AccountBalance` returns an error rather than a zero. Model capabilities
(`vision`, `reasoning`) come from the vendor's own `tags` and the measured
built-in table — never from guesswork.

---

## Where credentials come from

The module resolves credentials from these sources, in priority order. All of
them end up as `account` records in the pool.

1. **The module config** — either the single-account shorthand
   (`access_token`, `refresh_token`, `expires_at`, `office_identity`, `user_id`,
   `nickname`, `phone`, `device_id`, `label`) or an `accounts` array of the same
   shape, each with an extra `disabled` flag.
2. **Environment variables** — `CLIENT2API_RACCOON_ACCESS_TOKEN`,
   `_REFRESH_TOKEN`, `_USER_ID`, `_NICKNAME`, `_PHONE`, `_DEVICE_ID`,
   `_OFFICE_IDENTITY`. They are consulted only when the config object supplied
   no account at all, so a config always wins.
3. **An imported credential file** — `credential_paths` lists JSON files the
   operator declares. `Discover` reports what is there, `Import` loads it. The
   accepted shapes are a single credential object, an array of them, or an
   object with a `credentials` or `accounts` array. Each object is
   `{"access_token": "...", "refresh_token": "...", "user_id": "...", …}` using
   the field names listed above.
4. **A paste in the panel** — `AddAccount` takes the same fields through the
   credential form (`AccountFields`).

Anything imported or pasted is persisted under `Deps.DataDir`:

| File | Contents |
| --- | --- |
| `<DataDir>/accounts.json` | the credentials (mode 0600, written atomically) |
| `<DataDir>/state.json` | runtime health only: cooldowns, last error, last use, disabled flag. No secrets. |

If `Deps.DataDir` is empty the module keeps everything in memory and writes
nothing anywhere.

### Vendor-storage auto-discovery: probed, and NOT implemented

Before writing this module the machine was probed for an installed Raccoon Work
desktop client, so that a real well-known credential path could be discovered
rather than guessed:

- directories `%LOCALAPPDATA%`, `%APPDATA%`, `%USERPROFILE%`,
  `%USERPROFILE%\AppData\LocalLow`, `%LOCALAPPDATA%\Programs`, and the Start
  Menu (per-user and `ProgramData`) — name filter
  `raccoon|xiaohuanxiong|sensetime|sensenova|sn-`;
- a bounded recursive filename scan (depth 2) of `%LOCALAPPDATA%` and
  `%APPDATA%` for `raccoon|xiaohuanxiong|sensetime|小浣熊`;
- the registry uninstall keys
  `HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\*`,
  `HKLM:\…\Uninstall\*` and `HKLM:\Software\WOW6432Node\…\Uninstall\*`,
  filtering `DisplayName` on `raccoon|xiaohuanxiong|sensetime|小浣熊`.

**Every probe returned nothing**: no Raccoon Work desktop client is installed on
this machine. There is therefore **no verified path or storage key** to read, and
the module does not invent one. If the vendor client is installed elsewhere, its
storage location is *unverified* and the module will not look for it; use
`credential_paths` to point at the file once you have confirmed where it lives.

### How to obtain a credential by hand

1. Open the Raccoon Work desktop client (or `https://xiaohuanxiong.com`) and sign
   in normally.
2. Open developer tools → Network, and trigger any request to
   `xiaohuanxiong.com/api/web/…`.
3. Copy the `Authorization` header value and drop the leading `Bearer ` — that
   string is the `access_token`. If the client also stores a `refresh_token`
   (or you can see it in a `/api/web/auth/v1/refresh` request body), copy that
   too: **without a refresh token the account cannot renew** and it will die
   when the access token expires, which is roughly 3 hours.
4. Paste both into the panel's credential form, or put them in the config, or
   write them to a JSON file and list it in `credential_paths`.

The `office_identity` is a personal account's **empty** value, otherwise an org
code; it is sent as `X-Org-Code` on every request. The vendor's own `user_info`
reports `office_identity:""` for a personal account, and its chat endpoint
rejects the literal `personal` with `code 200022 org_not_found_error`, so any
`personal` sentinel is normalised to an empty header before it goes out.

---

## Login

`StartLogin` serves a small sign-in page from a `127.0.0.1` listener on a random
port and returns that URL; the panel opens it. The page has two tabs and talks
only to that loopback listener:

- **微信扫码** — the module generates a fresh 32-hex `qrcode_code`, renders it
  as an inline SVG QR (byte mode, ECC level M, hand-written encoder, no
  dependency) pointing at
  `https://xiaohuanxiong.com/login/mp?code=…&appname=商汤小浣熊官网`. `PollLogin`
  posts the code to `/api/web/auth/v1/login_with_qrcode_code` and maps the
  vendor states `pending` / `logging` / `canceled` / `success`. A `canceled`
  code is rotated and a fresh QR is returned; `success` is only accepted when
  the reply carries an access token, otherwise the session stays pending.
- **短信登录** — the operator solves the vendor's Aliyun slider captcha
  (`sceneId=1pkmy0x3`, `prefix=hk1r5l`, loaded from the vendor CDN only when
  this tab is selected), then submits the phone number and captcha param to
  `/raccoon/sms/send` and the code to `/raccoon/sms/verify`. The phone is
  encrypted host-side. A retryable SMS error is returned to the page and the
  session stays pending, so a mistyped code does not end the login.

On success the credential is enriched through `/api/web/auth/v1/user_info`,
written through the same pool as an imported token, and the desktop
`login-points` grant is fired once, best-effort.

**Security boundary.** The listener binds `127.0.0.1` only and serves exactly
`/raccoon/login`, `/raccoon/poll`, `/raccoon/sms/send` and
`/raccoon/sms/verify`. The page is sent `Cache-Control: no-store`, carries the
QR only as an inline SVG, and never receives the access/refresh token: polling
and the SMS exchange happen server-side in the module process. The session
expires after `login_timeout` (default 5m) and `CancelLogin` closes the
listener.

**The daily 300 credits still have no endpoint.** The vendor grants them
server-side (`daily_grant` in the points ledger); the only login-shaped reward
exposed here is the desktop `login-points` grant, which is not described as
daily.

---

## Endpoints, headers and encryption

All paths are relative to `https://xiaohuanxiong.com` (override with `base_url`).

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/api/web/auth/v1/refresh` | exchange `refresh_token` for a new access token |
| `GET` | `/api/web/auth/v1/user_info` | account metadata; used as the credential probe |
| `POST` | `/api/web/auth/v1/login_with_qrcode_code` | QR login poll (the WeChat scan); body `{"qrcode_code": "…"}`, unauthenticated |
| `POST` | `/api/web/auth/v1/send_sms` | request an SMS verification code; body `{"phone": "<encrypted>"}`, unauthenticated |
| `POST` | `/api/web/auth/v1/login_with_sms` | exchange the phone and `sms_code` for a credential, unauthenticated |
| `GET` | `/api/web/llm/v2/model_catalog` | model catalogue (chat category only) |
| `POST` | `/api/web/llm/v2/chat/completions` | streaming chat completions |
| `GET` | `/api/web/points/v1/balance` | credit balance (read-only) |
| `GET` | `/api/web/points/v1/bills?paging.limit=N&paging.offset=M` | credit ledger (read-only) |
| `POST` | `/api/web/desktop/v1/login/points/grant` | desktop login reward, action `login-points` (the `CheckinProvider`); the vendor's own hook fires it on every launch |

Headers on every request: `Accept: application/json` (or `text/event-stream` for
chat), `Content-Type: application/json`. Authenticated requests also carry
`Authorization: Bearer <access_token>`, `X-Org-Code: <office_identity or "">`
and `X-Raccoon-Language: zh`; the three login calls above are unauthenticated and
send none of those. When known, the
module also sends `X-Client-Platform` (`desktop-windows`), `X-Client-Version`
(`v1.0.35`) and `X-Client-Device-ID`. The vendor's own client identifies as
`Raccoon Work/1.0.35 (Windows)`, which is the default `User-Agent`.

### Phone transport encryption

Implemented as `encryptPhoneRandom` and pinned by a test. The SMS login path
uses it: the browser posts the plain phone number to the loopback page, which
encrypts it host-side before calling `/api/web/auth/v1/send_sms` and
`/api/web/auth/v1/login_with_sms`. The raw number is only kept if the vendor's
`user_info` returns it, as the display-only `phone` field (masked in the panel).

- key: `UTF8("senseraccoon2023")` — 16 bytes, so **AES-128**;
- random 16-byte IV, mode **AES-128-CFB**, padding **off**;
- output: `Base64(iv ‖ ciphertext)`.

The secret is a **public constant** hard-coded in the vendor's own front-end
bundle. It is not a security boundary; it is treated as log hygiene only.

### Envelope and errors

Non-streaming responses are `{code, message, details, data}`; `code == 0` is
success, and when `code` is absent the HTTP status decides (>= 400 is an error).
Errors are surfaced as `message` + `": " + details` prefixed `raccoon: `.
`HTTP 401`/`403` and envelope `code == 200003` all mean the refresh token is
dead: the account is marked invalid and a fresh sign-in is required.

### Renewal

`refresh_margin` defaults to 300 s. Expiry is resolved from `expires_at` when
that optional field parses to a positive number, and otherwise **from the JWT
`exp` claim**, decoded without verifying the signature and tolerating any parse
failure. A credential that carries no expiry information at all is treated as
**not** expired — the server's 401 is the authority. `expires_at` is a
millisecond timestamp string, matching the vendor client.

The renewal sweep (`refreshDue`) filters **only** on "has a refresh token",
never on the enabled flag: disabling an account stops it being selected for
traffic, it does not mean its credential may be allowed to lapse. This is the
reference's own headline defect and it is pinned by a test here.

---

## Model ids

`Models()` never blocks: it returns the cached catalogue, kicks a background
refresh when the cache is stale, and falls back to the built-in table below
until a live catalogue has been seen. Unrecognised ids are passed through
verbatim, and a display name (`Kimi-K3 · x1`) is accepted as an id too.

The catalogue comes from the `chat` category of `/api/web/llm/v2/model_catalog`.
Only entries with `visible: false` are filtered out, and duplicates are resolved
first-wins. If the fetch fails the module returns an empty list so the caller
falls back to the built-in table — a catalogue failure never breaks chat.

Built-in table (measured from the live catalogue; `max_tokens` feeds
`core.ModelLimitsProvider`):

| id | display name | context | max output | vision |
| --- | --- | --- | --- | --- |
| `sn-sensenova-6-8-flash` | `SenseNova-6.8-Flash · 免费` | 256000 | 63999 | yes |
| `sn-sensenova-6-8-flash-lite` | `SenseNova-6.8-Flash-Lite · 免费` | 256000 | 63999 | yes |
| `sn-glm-5-3` | `GLM-5-3 · x0.75` | 1000000 | 100000 | yes |
| `sn-kimi-k3` | `Kimi-K3 · x1` | 1000000 | 100000 | yes |
| `sn-glm-5-3-flash` | `GLM-5-3-Flash · x0.2→x0.1` | 1000000 | 100000 | no |
| `sn-deepseek-v4-1-flash` | `DeepSeek-V4.1-Flash · x0.25` | 1000000 | 100000 | no |

`Raccoon-Auto` is deliberately absent: it is a client-side "pick for me" entry in
the vendor's UI, not a remote model, and sending it to `chat/completions` 404s.

### Display-name multiplier rules

The billing multiplier is appended to the **name** (the model picker renders only
`name`), in this order:

1. effective multiplier missing, non-finite or negative → **no suffix at all**
   (never a dangling ` · `);
2. effective multiplier `0` → ` · 免费` (not `x0`);
3. base multiplier finite, positive and strictly greater than the effective one
   → ` · x<base>→x<effective>`;
4. otherwise → ` · x<effective>` — **including 1×**.

Rule 4 is load-bearing. Omitting the 1× case was a real user-reported defect:
without it the user cannot tell "this model is 1×" from "we failed to read the
multiplier". `stripMultiplier` removes the suffix so a display name can be fed
back in as an id.

---

## Configuration

The module reads the JSON object under `clients.raccoon` in
`configs/client2api.json`. **Every key is optional**; an absent object is the
same as `{}`, and a **malformed** object is logged and replaced with defaults —
a bad config is never a construction failure. See `config.example.json` for a
copy-paste starting point.

```jsonc
{
  "clients": {
    "raccoon": {
      "access_token": "<jwt>",
      "refresh_token": "<jwt>",
      "office_identity": "",

      "credential_paths": ["C:\\path\\to\\raccoon-credentials.json"],

      "accounts": [
        { "label": "work", "access_token": "<jwt>", "refresh_token": "<jwt>", "user_id": "12345" }
      ]
    }
  }
}
```

### Top-level keys

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `base_url` | string | `https://xiaohuanxiong.com` | API origin; a trailing `/` is trimmed |
| `user_agent` | string | `Raccoon Work/1.0.35 (Windows)` | `User-Agent` sent upstream |
| `client_platform` | string | `desktop-windows` | `X-Client-Platform` |
| `client_version` | string | `v1.0.35` | `X-Client-Version` |
| `accounts` | array | — | list of credential objects (see `accountConfig`) |
| `access_token` | string | — | single-account shorthand |
| `refresh_token` | string | — | single-account shorthand; enables renewal |
| `expires_at` | string | — | millisecond timestamp; omit to let the JWT `exp` decide |
| `office_identity` | string | `""` | org code, sent as `X-Org-Code`; blank for a personal account |
| `user_id` | string | — | groups credentials belonging to one account |
| `nickname` | string | — | display only |
| `phone` | string | — | display only (masked in the panel) |
| `device_id` | string | — | `X-Client-Device-ID` |
| `label` | string | — | display only |
| `credential_paths` | array | `[]` | operator-declared credential files for import |
| `max_attempts` | int | `3` | accounts tried per request before giving up |
| `models_ttl` | duration | `5m` | how long a fetched catalogue is considered fresh |
| `models_timeout` | duration | `20s` | timeout for the catalogue and refresh calls |
| `chat_timeout` | duration | `10m` | whole-request timeout for a completion |
| `idle_timeout` | duration | `2m` | abort a stream that sends nothing for this long |
| `first_token_timeout` | duration | `120s` | abort a stream that never produces a first frame |
| `cooldown` | duration | `60s` | park time after an auth failure (a client error parks nothing) |
| `short_cooldown` | duration | `15s` | park time after a transient/network failure |
| `quota_cooldown` | duration | `24h` | park time after a quota failure |
| `refresh_margin` | duration | `300s` | renew this long before expiry |
| `store_flush` | duration | `5s` | debounce for writing `accounts.json`/`state.json` |
| `login_timeout` | duration | `5m` | how long an interactive QR/SMS login stays pending before it expires |

Durations accept a Go duration string (`"90s"`, `"5m"`) or a bare JSON number
read as seconds. A value that cannot be parsed falls back to the default.

**A client error parks nothing.** The three durations above cover an auth
refusal (`cooldown`), a transient or network failure (`short_cooldown`) and a
quota exhaustion (`quota_cooldown`). A `kindClient` failure — a 4xx the vendor
rejected, a model id it does not serve, a local decode error — is the *request*
being wrong, so `cooldownFor` returns no park at all: the credential stays
selectable and `Status` keeps reporting `ready`, while the request loop still
refuses to replay it on another account (it would fail identically everywhere).
`TestAClientErrorNeverParksAHealthyAccount` pins this and
`TestAnAuthErrorStillParksTheAccount` pins that an auth failure still parks.

### Optional `core` interfaces

| Interface | Implemented | Notes |
| --- | --- | --- |
| `core.AccountManager` | yes | credential form, CRUD, test, per-account refresh |
| `core.CredentialImporter` | yes | over `credential_paths` only; no path guessing |
| `core.Reviver` | yes | clears cooldown/health/counters and re-enables; never mints a credential |
| `core.ModelRefresher` | yes | returns the last good list *alongside* the error |
| `core.ModelLimitsProvider` | yes | `max_tokens` from the catalogue cache; declines on a cold cache |
| `core.BalanceProvider` | yes | `GET /api/web/points/v1/balance`, read-only |
| `core.PackageProvider` | yes | the balance's pools as separate packages |
| `core.LoginProvider` | yes | loopback QR/SMS page; `RealmLoginProvider` is not implemented (a single sign-in flow) |
| `core.CheckinProvider` | yes | one action, `login-points`: the desktop login grant. The 300 daily credits have no endpoint and are not claimed |
| `core.VoucherProvider` | **no** | the vendor exposes no voucher concept |
| `core.TaskProvider` / `core.BatchProvider` | **no** | nothing to offer |
| `core.CaptchaProvider` | **no** | the SMS tab loads the vendor's Aliyun slider in the browser; the module never solves a captcha server-side |
| `core.HintProvider` | **no** | nothing to hint at |

`TestAccount` makes one real, read-only call to `/api/web/auth/v1/user_info`.
A refusal is reported as a failed test result, not as an error.

---

## Tests

```sh
gofmt -l ./clients/raccoon        # must print nothing
go vet ./clients/raccoon/         # must exit 0
go test -count=1 -timeout 20m ./clients/raccoon/
```

The suite is fixture-driven — `httptest` servers and stub `http.RoundTripper`s,
never the real network. It covers config parsing and defaults, credential
persistence round-trips, the JWT-`exp` fallback (including a credential with no
`expires_at` and a malformed token), the refreshable/expired/refresh-due
predicates, the refresh request shape and its refresh-token carry-over, the phone
cipher against a pinned vector, the display-name multiplier rules, the catalogue
parse, SSE framing and chunk parsing, tool-call fragment merging, the
whole-JSON-completion reframing, the `ErrNotConfigured` path, and the check-in
suite (`checkin_test.go`): the action list with and without accounts, the grant
request's headers and empty-JSON body, envelope unwrapping, a popup-carrying
grant, `granted:false` as a refusal rather than an error, an unknown action
refused without a call, an unknown account id as a real error, a rejected token
parking the account, a *client* error not parking it, the token never leaking
into the error text, and one grant per click.

`login_test.go`, `login_sms_test.go` and `qr_test.go` cover the loopback page
and its security boundary, the QR state machine (`pending`/`logging`/
`canceled`/`success`, code rotation, a tokenless `success` staying pending),
the SMS send/verify endpoints and their retryable-error handling, and the
stdlib QR encoder against fixed vectors (including an over-capacity error).

---

## Unverified claims and deviations from the reference

Marked honestly rather than asserted:

- **No live call was ever made to `xiaohuanxiong.com` from this module.** Every
  endpoint shape, header name, field name and multiplier rule above is taken
  from the reference implementation and its recorded measurements, not from a
  request this module made. The `points/v1/bills` query-parameter spelling
  (`paging.limit` / `paging.offset`) and the `desktop/v1` grant behaviour are the
  least exercised of these.
- **The access-token lifetime (~3 h) and the 300 s renewal window** come from the
  reference's measurements; the module only relies on the JWT `exp`.
- **`max_output_tokens` values** are the vendor's `params.max_tokens` as recorded
  by the reference; if the vendor changes them, the live catalogue wins and the
  built-in table is only a fallback.
- **Deviation:** the reference's QR/SMS sign-in is ported, but the QR encoder is
  hand-written in `qr.go` (byte mode, ECC level M, versions 1-10) so no
  dependency is added; the reference used a QR library. No live scan has been
  performed from this machine.
- **Deviation:** the reference exposes a check-in-shaped credits view. This module
  exposes one action, `login-points`, wired to the desktop login grant, and leaves
  the daily 300 credits alone — they have no endpoint.
- **The reward window is unknown, and the code says so.** The grant reply is
  `{granted, popup?}`: no reason, no "next available at", and the client never
  states how often the server grants. Earlier drafts of this module asserted "at
  most one grant per account per day"; that claim was **removed** because nothing
  in the vendor bundle supports it. `TestCheckinTreatsNotGrantedAsARefusalNotAnError`
  now fails if the message ever claims such a window again.
- **Deviation:** the reference can read the desktop client's own credential
  storage; this module does not, because no such storage could be verified on
  this machine. `credential_paths` is the substitute.
- **Deviation:** the reference caches the whole catalogue including
  blacklist-filtered entries; this module returns the visible `chat` models.
