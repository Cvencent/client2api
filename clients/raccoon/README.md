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

**The daily 300 credits are extended through the desktop login grant.**
The endpoint is `POST /api/web/desktop/v1/login/points/grant` (action id
`login-points`). The vendor's own client fires it from a React hook on every
launch and shows a popup only when the reply says `granted:true`.

The endpoint does **not** mint the day's 300 points — it stretches their
expiry. The vendor's own wording states the rule: 每日积分每天 00:00 发放，默认
当日 24:00 清零，当天登录后有效期延长至 7 天. The ledger matches: the 00:00
issuance is a `daily_grant` of `+300` expiring **the same day**, and a
successful desktop grant adds another `daily_grant` of `+300` whose `expire_at`
reaches **seven days** out.

**The vendor only hands this reward to its own desktop app.** The request that
app sends is a plain authenticated POST — the same method, URL, headers and
empty body this module sends — but replaying it from the gateway is answered
with `{"code":0,"data":{"granted":false}}` for **every** account and every
variation tried (device id, client version, `Origin`, cookies, user agent, org
header, body shape). What gives the caller away is not the request: a real
account's ledger shows the extension landing on exactly 10/08 18:41, 10/16
08:22, 10/20 18:12 and 10/23 21:41, each within seconds of that app's own
`app-startup` log line, and on 10/20 this gateway's 07:00 attempt was refused
while the same account's 18:12 attempt — fired by the app — was granted. The
discriminator is the caller, not the request and not the clock.

`granted:false` is therefore the **settled** answer for a gateway call, not a
transient one, and the module records it as `skipped` (the panel greys the row
out until the next Shanghai midnight) instead of reading as a failed request.
The ledger is still consulted, for the one benign reading of a refusal:

| Ledger state | Verdict |
| --- | --- |
| today's `daily_grant` expires beyond today | already claimed — `OK` with `data.already_done = true` |
| only the same-day 00:00 issuance | desktop-only refusal — `skipped`, `data.needs_desktop_client = true` |
| ledger unreadable | desktop-only refusal — `skipped`, `data.needs_desktop_client = true` |

An earlier revision treated every `granted:false` as a retryable refusal, on the
theory that the vendor kept handing the extension out later in the day. It does
— to its own desktop app. Read from the gateway the same request is refused all
day long, so leaving the row enabled only produced a button whose every click
failed, and the batch runs counted it as a refusal every morning and evening.

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
  (`sceneId=1pkmy0x3`, `region=cn`, `prefix=hk1r5l`, loaded from the vendor CDN
  only when this tab is selected), then submits the phone number and captcha
  token to `/raccoon/sms/send` and the code to `/raccoon/sms/verify`. The phone
  is encrypted host-side. A retryable SMS error is returned to the page and the
  session stays pending, so a mistyped code does not end the login.
  Both legs run against the IAM service: `POST /api/iam/v1/sms/send`
  (`captcha_token`, `scene=login`) answers with a `verification_id`, and
  `POST /api/iam/v1/auth/login/sms` quotes that id beside the `verification_code`.
  The legacy `/api/web/auth/v1/send_sms` and `/login_with_sms` are **dead**
  (they answer every login request with a captcha failure), which is why the
  send/verify pair moved to IAM. QR and token refresh stayed on the web API.
  IAM hands back an identity token for a business session that still has to be
  **confirmed**: the vendor's own web client follows the login with
  `POST /api/web/auth/v1/iam/confirm` (empty JSON body, same bearer token) and
  replays any business call that answers `400` + code `200901`. Skipping it
  makes a brand-new account look broke — balance, `user_info` and the model
  catalog all refuse with `iam_confirm_required`, and the credential is stored
  as a nameless bare token. The module confirms before the profile read and
  retries once on `200901`, so accounts stored before the fix heal themselves.
- **一键接码 / 自动登录 — 已关闭**（deliberately not offered).  This module used
  to implement `core.AutoLoginProvider`: rent a number from the eomsg platform
  (`sms_token`), mint the Aliyun slider parameter in a local Chromium-family
  browser, send the number through the vendor's SMS API, read the code back and
  store the credential.  It no longer advertises that capability, so the panel
  hides the 一键接码 button.

  Why: the vendor moved SMS login to the IAM service **and** hardened the Aliyun
  scene so the traceless fast path no longer auto-passes.  Measured against the
  live site, their own login page now mounts an interactive slider that needs a
  human to drag it, and a parameter minted locally — in a real browser, from the
  vendor's own origin, in the same session, with the same cookies — is rejected
  with `captcha_token invalid`.  An unattended run therefore cannot obtain a
  usable token, so the button could only ever fail.  The manual 接码 controls
  above are unaffected: they still rent a number and read the code, while the
  operator solves the slider in the vendor's own page.

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

**The daily 300 credits ride on the desktop `login-points` grant.** The vendor
writes them to the points ledger as `daily_grant`; the panel's check-in button
calls `POST /api/web/desktop/v1/login/points/grant` to extend the day's 300.
The vendor only grants it to its own desktop app, so a `granted:false` is the
settled answer for a gateway call: the module resolves it against the ledger
(see the overview) and records it as "unavailable — claim it from the desktop
client" unless the ledger proves today's extension is already in hand.

---

## Endpoints, headers and encryption

All paths are relative to `https://xiaohuanxiong.com` (override with `base_url`).

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/api/web/auth/v1/refresh` | exchange `refresh_token` for a new access token |
| `GET` | `/api/web/auth/v1/user_info` | account metadata and credential enrichment |
| `POST` | `/api/web/auth/v1/login_with_qrcode_code` | QR login poll (the WeChat scan); body `{"qrcode_code": "…"}`, unauthenticated |
| `POST` | `/api/web/auth/v1/send_sms` | request an SMS verification code; body `{"phone": "<encrypted>"}`, unauthenticated |
| `POST` | `/api/web/auth/v1/login_with_sms` | exchange the phone and `sms_code` for a credential, unauthenticated |
| `GET` | `/api/web/llm/v2/model_catalog` | model catalogue (chat category only) |
| `POST` | `/api/web/llm/v2/chat/completions` | streaming chat completions |
| `GET` | `/api/web/points/v1/balance` | credit balance (read-only) |
| `GET` | `/api/web/points/v1/bills?paging.limit=N&paging.offset=M` | credit ledger (read-only) |
| `POST` | `/api/web/desktop/v1/login/points/grant` | desktop login reward, action `login-points` (the `CheckinProvider`): extends the day's 300 points to a 7-day expiry. The vendor grants it to its own desktop app alone, so a gateway call is answered `granted:false` and resolved against the ledger above |

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

## Free models

A model whose effective `billing_multiplier` is zero -- including a
limited-time campaign that prices a model at zero while its list price stays
paid -- carries `extra.free`.  The router marks the attempt with
`core.ChatRequest.FreeModel` and the account guard steps aside for it: a
credential parked by the vendor's quota verdict still serves it, because it
spends no credit.  A dead session, a rate limit or the operator's disable
switch keeps applying, and so does a per-pair park.

A free model the vendor refuses on credit grounds parks that
(credential, model) pair alone; the credential keeps serving its other free
models, and a free success never lifts the wallet park.

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
| `sms_token` | string | — | eomsg (接码平台) credential for the manual 接码 controls |
| `sms_keyword` | string | `商汤科技` | sender keyword the platform filters on; a wrong keyword reads as "no message" |
| `sms_base` | string | eomsg | platform endpoint override |
| `sms_provinces` | array | built-in pool | province rotation; empty keeps the built-in pool, `["none"]` lets the platform choose |
| `sms_card_type` | string | `全部` | eomsg card class: `实卡`, `虚卡` or `全部` |
| `dup_retries` | int | `6` | extra draws when the platform returns a virtual or already-used number |

`browser_path`, `browser_headless`, `auto_login_timeout_seconds`, `sms_polls`
and `sms_interval_seconds` were removed along with the auto-login flow. A config
that still carries them keeps loading — unknown keys are ignored — but they no
longer do anything.

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

A credential at its per-account in-flight ceiling is skipped before any vendor
call: it is not parked and it does not spend one of the `max_attempts` attempts.

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
| `core.SMSProvider` | yes | eomsg rent/poll/release; sender keyword defaults to `商汤科技` |
| `core.AutoLoginProvider` | **no** | deliberately dropped: the vendor's Aliyun scene now needs a human slider, so an unattended 一键接码 run cannot get a usable captcha token |
| `core.CheckinProvider` | yes | one action, `login-points`: extends the daily 300 login credits through the desktop login grant. The vendor only grants it to its own desktop app, so a `granted:false` resolves against the points ledger into either "already claimed" or a settled "unavailable — claim it from the desktop client" |
| `core.VoucherProvider` | **no** | the vendor exposes no voucher concept |
| `core.TaskProvider` / `core.BatchProvider` | **no** | nothing to offer |
| `core.CaptchaProvider` | **no** | the interactive SMS tab loads the vendor's Aliyun slider in the browser for the operator to solve |
| `core.HintProvider` | **no** | nothing to hint at |

`TestAccount` sends one minimum-size real chat completion through the account under
test, so a token that opens the gateway but cannot answer is reported as failed.
Thinking is explicitly disabled with top-level `reasoning_effort: "none"`:
SenseNova's default thinking can consume the entire 16-token budget as
`reasoning_content`, which would otherwise look like an empty answer and wrongly
park the account. A cleanly completed stream carrying reasoning or a tool call
is still valid proof that the credential works. A stream error after partial
reasoning remains a failed test, as does a stream with no completion evidence at
all. A refusal is reported as a failed test result, not as an error.

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
grant, `granted:false` as a settled unavailable rather than an error, the
refusal-resolution rule (a completed extension in the ledger reads as already
claimed; the un-extended 00:00 issuance and an unreadable ledger both read as
the desktop-only unavailable), the ledger predicate pinned against rows copied
from a real account, an unknown action refused without a call, an unknown
account id as a real error, a rejected token parking the account, a *client*
error not parking it, the token never leaking into the error text, and one
grant per click.

`login_test.go`, `login_sms_test.go` and `qr_test.go` cover the loopback page
and its security boundary, the QR state machine (`pending`/`logging`/
`canceled`/`success`, code rotation, a tokenless `success` staying pending),
the SMS send/verify endpoints and their retryable-error handling, and the
stdlib QR encoder against fixed vectors (including an over-capacity error).

---

## Unverified claims and deviations from the reference

Marked honestly rather than asserted:

- **Live checks were run against `xiaohuanxiong.com` for the points surface.**
  `GET /api/web/points/v1/balance`, `GET /api/web/points/v1/bills` (with
  `paging.limit` / `paging.offset`) and `POST /api/web/desktop/v1/login/points/grant`
  were all exercised with a real account. The grant endpoint's request shape is
  measured against the vendor's own installed client (`app.asar`): same method,
  URL and headers. Its **reply** is measured too — every replay from this
  gateway, including with the desktop client's own stored token, the real
  `X-Client-Device-ID`, both `Origin` values, cookies and several client
  versions, came back `granted:false`. The `daily_grant` of `+300` with a
  seven-day expiry is in the ledger, but only at the timestamps where the real
  desktop app had just started. The chat/streaming endpoints remain ported from
  the reference.
- **The access-token lifetime (~3 h) and the 300 s renewal window** come from the
  reference's measurements; the module only relies on the JWT `exp`.
- **`max_output_tokens` values** are the vendor's `params.max_tokens` as recorded
  by the reference; if the vendor changes them, the live catalogue wins and the
  built-in table is only a fallback.
- **Deviation:** the reference's QR/SMS sign-in is ported, but the QR encoder is
  hand-written in `qr.go` (byte mode, ECC level M, versions 1-10) so no
  dependency is added; the reference used a QR library. No live scan has been
  performed from this machine.
- **2026-10-27 deep dive (CDP + wire)**: the desktop app was launched with a debug port and its
  startup grant observed; an in-renderer replay of the grant (same session, same origin, same TLS)
  on a *fresh* account is also refused; `/api/web/auth/v1/devices_current_bind` +
  `devices_current_heartbeat` (both answer `trusted_device:true`) do **not** unlock it either.
  The desktop client mints its session through its own local
  `/api/electron/auth/v1` proxy at login, so the server-side marker the grant checks is a property
  of how the session was created, not of anything request-shape or device-state can add afterwards.
  Token rotation note: refreshing a session out-of-band invalidates the previous access token
  immediately (the vendor rotates on refresh), so probes must persist the rotation like the module does.
- **2026-10-27 root cause found: the missing device id.** The grant is answered `granted:false` exactly for accounts whose stored credential carries **no** `device_id` (token-dump imports) and
  granted for accounts that carry one — 4/4 correlation across the pool, plus a bind+heartbeat probe on
  a device-less account still refused. `loadLocked` now mints and persists a device id for any stored
  credential that lacks one, so the `X-Client-Device-ID` header rides on every request from the first
  restart on. (The in-app replay result stands: inside the real client the same call is refused for an
  account whose daily grant was already taken — that reading is `already_done`, not the gate.)
- **The desktop gate itself is unexplained; that it exists is measured.** The
  vendor's own desktop client sends the same request this module sends, yet
  only that client is granted the extension. Whatever the server keys on (a
  desktop-bound session, a device link, an online presence) is not in the
  request bytes, so this module does not pretend to reproduce it: it reports
  the settled refusal instead.
- **Deviation:** the reference exposes a check-in-shaped credits view. This
  module exposes one action, `login-points`, wired to the desktop login grant.
  From this gateway that grant is refused (see above), so the action reads as
  "unavailable — claim it from the desktop client" rather than as a claim that
  succeeded.
- **The reward window is unknown; the expiry is not.** The grant reply is
  `{granted, popup?}`: no reason and no "next available at", so the module
  never invents one. What it does read is the ledger's own `expire_at`, which is
  what separates "the extension already happened" from "it has not yet".
  Earlier drafts asserted "at most one grant per account per day", then that a
  mid-day refusal was merely "not final for the day"; both claims were
  **removed** once the ledger and the desktop app's own launch log showed that
  the discriminator is the caller, not the hour.
  `TestCheckinRefusalIsSettledForTheDay` fails if the verdict drifts back to
  "retry later".
- **Deviation:** the reference can read the desktop client's own credential
  storage; this module does not, because no such storage could be verified on
  this machine. `credential_paths` is the substitute.
- **Deviation:** the reference caches the whole catalogue including
  blacklist-filtered entries; this module returns the visible `chat` models.
