# client2api / clients/openrouter

An [OpenRouter](https://openrouter.ai) client module for `client2api`.

OpenRouter is a multi-vendor **model router** that speaks the OpenAI
chat-completions protocol.  One static API key reaches ~460 models from
OpenAI, Anthropic, Google, DeepSeek, Qwen, Mistral, xAI, Meta, and others, so
this module is deliberately thin: it authenticates with a bearer key, streams
`POST /chat/completions`, and reads the vendor's own catalogue for context
windows and output limits.

* Base URL: `https://openrouter.ai/api/v1`
* Auth: `Authorization: Bearer <OPENROUTER_API_KEY>`
* Inference: `POST /chat/completions` (SSE streaming)
* Catalogue: `GET /models` (public, no auth)
* Balance: `GET /credits` (management keys) with `GET /key` as the fallback

An OpenRouter credential is a static API key, but the module can obtain one for
you through OpenRouter's browser **PKCE login** (see "Login" below) — the
Add Account flow opens the vendor's consent page and stores the key it issues.
There is no key to refresh, no check-in and no task board, so this module
implements no `Reviver`, `CheckinProvider`, `TaskProvider` or `BatchPlanner`.

---

## Routing: the model id keeps its slash

The gateway routes a request as `<client>/<model>` and splits on the **first
slash only** (`internal/core/registry.go` uses `strings.Cut(model, "/")`).
OpenRouter model ids themselves contain a slash, so the wire form keeps the
vendor id intact:

```
{"model": "openrouter/anthropic/claude-sonnet-4.5", ...}
```

resolves to this module with `req.Model == "anthropic/claude-sonnet-4.5"`, which
is exactly the id `POST /chat/completions` expects.  **Do not double-qualify.**
The catalogue this module returns (`Models()`, `Status().Models`, `GET
/v1/models`) always carries the **unqualified vendor id** —
`anthropic/claude-sonnet-4.5`, never `openrouter/anthropic/claude-sonnet-4.5`.
The router pseudo-models (`openrouter/auto`, `openrouter/free`, …) are vendor
ids in their own right, so `openrouter/openrouter/auto` is the correct wire
form for them.

---

## Configuration

`Deps.Config` is the raw JSON of the `clients.openrouter` object.  A missing or
`null` config is fine; a malformed one is logged and the defaults are used (a
module that refuses to be constructed would vanish from the panel, where it
cannot be repaired).  See `config.example.json`.

| key | default | meaning |
| --- | --- | --- |
| `base_url` | `https://openrouter.ai/api/v1` | API root (no trailing slash) |
| `api_key` | — | the primary credential |
| `accounts` | `[]` | `{id, label, api_key, disabled}` — additional keys |
| `http_referer` | repo URL | optional `HTTP-Referer` attribution header |
| `x_title` | `client2api` | optional `X-Title` attribution header |
| `user_agent` | `client2api/1.0 (+openrouter)` | `User-Agent` |
| `extra_headers` | `{}` | extra request headers (reserved names are dropped) |
| `extra_models` | `[]` | extra ids appended to the built-in fallback catalogue |
| `chat_timeout` | `10m` | whole-request deadline for `POST /chat/completions` |
| `models_timeout` | `30s` | deadline for catalogue/balance GETs |
| `stream_idle_timeout` | `90s` | max gap **between** two SSE frames |
| `models_ttl` | `10m` | how long a fetched catalogue is considered fresh |
| `cooldown` | `60s` | park time after unclassified failures (3 in a row) |
| `rate_cooldown` | `2m` | park time after a plain 429 |
| `quota_cooldown` | `30m` | park time after an out-of-credit 402 |
| `auth_cooldown` | `30m` | park time after a refused key (also marks it invalid) |
| `max_in_flight` | `4` | concurrent requests per credential (`0` = module default) |
| `max_tokens_field` | `max_completion_tokens` | set to `max_tokens` for a legacy deployment |
| `test_model` | `openai/gpt-4o-mini` | model used by the panel's "Test" button |
| `auth_url` | `https://openrouter.ai/auth` | browser entry point of the PKCE login |
| `login_timeout` | `5m` | how long a started PKCE login stays pending before it expires |

Durations are Go duration strings.  An empty, unparseable or non-positive value
falls back to the default rather than silently disabling a guard.
`extra_headers` may not override `Authorization`, `Content-Type`, `Accept`,
`User-Agent`, `Host`, `Content-Length`, `Transfer-Encoding`, `Connection`,
`HTTP-Referer` or `X-Title`.

`HTTP-Referer` and `X-Title` are the two attribution headers OpenRouter
documents.  They are **optional**; they only affect the app's ranking on
openrouter.ai and nothing about the request itself.

---

## Credentials

Precedence, highest first:

1. **module config** — `api_key`, then each `accounts[]` entry,
2. **`OPENROUTER_API_KEY`** in the process environment,
3. **the module store** — `data/openrouter/credentials.json`, written at mode
   0600 by `core.WriteJSONAtomic`.

The same key is the same account however it was supplied: records are keyed by
`keyFingerprint(key)`, so listing a key in the config *and* in the environment
does not create two accounts.  Config and environment keys are durable where
they already live and are never copied into the module store; only keys added
from the panel (or imported) are persisted there.

`Discover` reports, read-only, the module store, `env:OPENROUTER_API_KEY`, and
the conventional paths `~/.config/openrouter/credentials.json`,
`~/.openrouter/credentials.json`, `%APPDATA%\openrouter\credentials.json`.
`Import` accepts `env:NAME` as a path as well as a JSON file, and understands a
bare string, `{"api_key": …}`, nested `{data|credentials|accounts|openrouter|
result: …}`, or any `sk-or-…` literal found in the text.

Health (disabled flags, cooldowns, last error, failure count) lives in
`data/openrouter/state.json`, whose record type has **no key field at all** —
so "a health file cannot leak a credential" is structural, not a promise.

---

## Login

`StartLogin` runs OpenRouter's browser **PKCE** flow and stores the key it
issues. `RealmLoginProvider` is not implemented because the vendor exposes a
single sign-in path.

1. The module binds an ephemeral `127.0.0.1` listener and generates a 32-byte
   PKCE verifier, its `S256` challenge, and a random `state`.
2. `LoginState.URL` is the `auth_url` page carrying `callback_url` (the
   loopback address), `code_challenge`, `code_challenge_method=S256`,
   `key_label` and `state`. The operator opens it and approves access.
3. The vendor redirects to the loopback listener with `code`. The handler
   rejects a callback whose `state` does not match, so a stray or replayed
   redirect cannot complete the session.
4. `PollLogin` exchanges the code at `POST /auth/keys` with the verifier and
   writes the returned key through the same pool / `credentials.json` /
   `state.json` path as a pasted key. Nothing is written on failure.

The listener is loopback-only, a session expires after `login_timeout`
(default 5m), and `CancelLogin` closes the listener idempotently.

### Use your own key instead

A key the PKCE login issues is scoped to this application, so OpenRouter
applies **this app's** rate and credit limits to it.  A key from the operator's
own account page is limited by that account instead and spends that account's
free quota, which is usually the better deal.  The module advertises that path
through `core.DirectKeyProvider` (`direct_key` in the panel's capabilities), so
the panel renders a one-field "use your own API key" box next to the login
button.  It posts to the ordinary `AddAccount` path — the same one the generic
"manual" form uses — so both routes share one validation and one store.

---

## Free models

`free_only` defaults to **on**: the module exists here to spend OpenRouter's
free quota, so a paid model has to be opted into rather than reached by
accident.  While it is on,

* the served catalogue is narrowed to the zero-cost ids, so the gateway's
  `/v1/models` and the panel's chat-test picker only offer free models;
* a chat that names an id the catalogue knows is paid is refused with
  `ErrUnsupported` before any upstream call, so an explicitly prefixed id
  cannot spend paid credit;
* a configured `test_model` is only honoured while it is itself free, because
  the built-in default (`openai/gpt-4o-mini`) is paid.

Set `"free_only": false` to get the vendor's whole catalogue back.

`GET /models` publishes `pricing.prompt` and `pricing.completion` as strings. A
model is marked `Extra["free"] = true` only when **both** directions parse to
zero — a model that is free to prompt but charged per completion token is not
free, and one that publishes no pricing is unknown rather than free. Before the
first live catalogue arrives, the module falls back to a cold-start heuristic:
a `:free` id suffix, or the `openrouter/free` alias.

`TestAccount` probes with `test_model` when set (and free, under `free_only`),
otherwise the first cached model the catalogue marks free, otherwise the
`openrouter/free` alias — so the Test button probes a free model instead of a
paid one.

---

## Endpoints used

| call | auth | purpose |
| --- | --- | --- |
| `POST /chat/completions` | yes | inference, SSE when `stream: true` |
| `GET /models` | no | catalogue: `context_length`, `top_provider.max_completion_tokens` |
| `GET /credits` | yes | `total_credits`, `total_usage` (USD) |
| `GET /key` | yes | per-key `limit`, `limit_remaining`, `usage` |

* `GET /chat/completions` is 404 — the inference path is POST-only.
* The error envelope is `{"error":{"message": "<text>", "code": <int>,
  "metadata": {…}}}`.  It is **not** the OpenAI envelope, and it is parsed as
  such.
* `pricing.prompt` / `pricing.completion` are JSON **strings** in `GET /models`
  (`"0.000003"`), so they are decoded via `json.Number`, never as a float.
* `GET /credits` requires a **management** key; a plain inference key gets
  `403 {"error":{"code":403,"message":"Only management keys can perform this
  operation"}}`.  That is why `AccountBalance` tries `/credits` first and falls
  back to `/key` (`limit_remaining`), and why a 403 there is not an error.
* `GET /key`'s `label` is itself a masked key (`sk-or-v1-au7...890`); any label
  starting `sk-or-` is masked again before it could reach the panel.
* `GET /generation` needs `?id=<generation id>` and is not used.
* `GET /models/{author}/{slug}/endpoints` is public per-provider detail; it is
  not needed, because `GET /models` already publishes the output limit.

The PKCE login touches two more endpoints:

* `GET https://openrouter.ai/auth` — the browser consent page, opened with
  `callback_url`, `code_challenge`, `code_challenge_method=S256`, `key_label`
  and `state`; the vendor redirects back to the loopback listener with `code`.
* `POST /auth/keys` (under `base_url`) — exchanges `code` + `code_verifier`
  for a freshly minted API key. It is **unauthenticated**: the code is the
  credential.

---

## Interfaces

Implemented:

| interface | where |
| --- | --- |
| `core.Client` | `openrouter.go` |
| `core.AccountManager` | `accounts.go` |
| `core.CredentialImporter` | `credential.go` |
| `core.ModelRefresher` | `models.go` |
| `core.ModelLimitsProvider` | `models.go` |
| `core.BalanceProvider` | `balance.go` |
| `core.PoolStatsReporter` | `openrouter.go` |
| `core.HealthProvider` | `openrouter.go` |
| `core.LiveReloader` | `openrouter.go` |
| `core.LoginProvider` | `login.go` (browser PKCE) |
| `core.DirectKeyProvider` | `accounts.go` (`api_key`) |

Deliberately **not** implemented, with the reason:

* `Reviver`, `RealmLoginProvider`, `CaptchaProvider` — a key obtained by the PKCE
  login never expires on its own, so there is nothing to refresh or revive, and
  the flow is a single realm with no captcha.
* `CheckinProvider`, `TaskProvider`/`TaskAccepter`/`TaskClaimer`/
  `TaskAutoRunner`, `BatchPlanner`, `VoucherProvider`, `PackageProvider` — the
  vendor has no check-in, no task board, no vouchers and no credit packages.
* `ConversationBinder` — a single upstream endpoint makes stickiness
  meaningless; the module always talks to the same host.
* `BundleImporter` — OpenRouter has no exported bundle format.
* `HintProvider`, `Degrader` — the pool is a handful of keys; the built-in
  cooldown ladder already covers it.

### `ModelMaxOutputTokens` never fetches

`ModelMaxOutputTokens` is called on every chat request that omitted
`max_tokens`, so it answers **only** from the in-memory catalogue
(`core.OutputLimitFor(c.statusModels(), model)`).  A cold cache declines
(`ok == false`) rather than issuing a metadata round trip in the middle of a
turn.  The value comes from `top_provider.max_completion_tokens`, which 455 of
the 462 live models publish; the 7 router pseudo-models publish none and
therefore decline.  Nothing is ever guessed: too high is a vendor rejection,
too low is silent truncation.

### Live reload

`ApplyLive` honours `max_in_flight`, `pool.breaker_threshold` and
`pool.breaker_cooldown`.  It ignores `max_in_flight_global` (OpenRouter has no
realm split, so there is no second pool to cap) and the affinity knobs (no
`ConversationBinder`).

---

## Tests

```
gofmt -l clients/openrouter
go vet ./clients/openrouter/
go test -count=1 ./clients/openrouter/
```

Everything is offline: `openrouter_test.go`, `chat_test.go` and
`accounts_test.go` drive a fake `http.RoundTripper` (`fakeUpstream`) that
records every request and body, plus an `httptest`-free SSE fixture set and a
"stalled stream" body for the idle-timeout path.  There is no live test file,
because there is no OpenRouter credential on the machine this was written on
(see below).

---

## Verified vs assumed

This module was written from recorded reconnaissance, not from a credentialled
session.  Nothing below claims more than it can show.

**Verified by parsing the recorded live responses** in `client2api/run/`
(`or-models.json`, 759,828 bytes; `or-openapi.json`, 2,566,423 bytes).  Those
files were captured by earlier live probes on this machine; they were parsed and
measured here, not re-fetched:

* `GET /models` is public, returns `{"data":[…]}`, and the recorded body holds
  **462** models (`total_count: 462`).
* `pricing.prompt` is a JSON string in all 462 rows.
* 455 of 462 rows carry `top_provider.max_completion_tokens`; 16 ids end in
  `:free`; no row has a null/zero `context_length`.
* The 7 rows without an output limit are exactly the router pseudo-models and
  one router plugin: `typesafe/jev-router`, `openrouter/auto-beta`,
  `openrouter/fusion`, `openrouter/pareto-code`, `openrouter/free`,
  `openrouter/bodybuilder`, `openrouter/auto`.
* `anthropic/claude-sonnet-4.5` → `context_length` 1000000,
  `top_provider.max_completion_tokens` 64000, `is_moderated` true,
  `knowledge_cutoff` `"2025-01-31"`, and both `max_tokens` and
  `max_completion_tokens` in `supported_parameters`.
* The OpenAPI document declares exactly two security schemes (`apiKey`,
  `bearer`), both `{type: "http", scheme: "bearer"}`.
* `GET /credits` → `{"data":{"total_credits":<double>,"total_usage":<double>}}`
  and its 403 example is the management-key message quoted above.
* `GET /key` → the 22 documented fields, including `limit_remaining`,
  `limit`, `usage`, `is_free_tier`, `free_model_daily_requests`, and a
  `rate_limit` object the spec itself marks deprecated.
* The error envelope `{error:{code,message,metadata}}` and the
  `ChatFinishReasonEnum` `["tool_calls","stop","length","content_filter",
  "error",null]`.
* `ChatStreamDelta`'s reasoning field is `reasoning`, **not**
  `reasoning_content`; `ChatStreamOptions.include_usage` is documented as
  deprecated and a no-op ("full usage details are always included").
* `POST /chat/completions` is the inference path and `GET` on it is 404; an
  unauthenticated POST answers `401 {"error":{"message":"No cookie auth
  credentials found","code":401}}`.

**Assumed, not verified:**

* The shape of a *successful* streaming response.  `ChatStreamChunk` and
  `ChatStreamDelta` are taken from the OpenAPI document, and the SSE reader was
  written to tolerate framing variations (CRLF or LF, `data:` with or without a
  space, multi-line data, comment keep-alives, a missing final blank line, a
  bare JSON line, a clean close with no `[DONE]`).  No real OpenRouter stream
  was ever observed.
* The behaviour of `GET /credits` and `GET /key` against a real key beyond the
  documented examples: the 200 shapes, the 403 on `/credits` for a plain key,
  and the `limit_remaining: null` case are all read out of the spec.  The
  fallback logic is therefore written defensively — a 403 from `/credits` is
  treated as "ask `/key` instead", and an unparseable answer is reported as an
  error rather than shown as a fabricated zero.
* Cooldown durations (`rate_cooldown` 2m, `quota_cooldown` 30m,
  `auth_cooldown` 30m, the midnight-bounded daily-cap park) are this module's
  own policy.  The vendor publishes no retry schedule.
* Which error text the vendor uses for a moderated prompt.  `blockedMarkers`
  covers the spellings seen in the spec and in OpenRouter's public docs
  ("flagged", "moderation", "content policy", …); a 403 carrying moderation
  text is classified as blocked rather than auth, which is the safer default.

**Never done on this machine:**

* **There is no `OPENROUTER_API_KEY` here, so no authenticated call was ever
  made** — not to `/chat/completions`, not to `/credits`, not to `/key`.  Every
  test in this module runs against a fake transport.  The module has never
  served a real OpenRouter request end to end.
* **The PKCE login has never been completed against the live vendor.** The
  callback handler, the `state` check and the `POST /auth/keys` exchange are
  pinned against `httptest` servers and a fake transport only; the real
  `auth_url` page and its redirect back to the loopback listener are untested.
* The live `GET /models` call was not re-issued; the recorded body was parsed
  instead.  A cold start with no network therefore falls back to the curated
  table in `models.go` (87 well-known ids with real context/output limits).
