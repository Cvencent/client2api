# `kilocode` client

A client module for the **Kilo AI Gateway** (`api.kilo.ai/api/gateway`), the
multi-vendor router behind Kilo Code.  It is OpenRouter-shaped: `provider/model`
ids, an OpenAI chat-completions surface, a public `GET /models` catalogue, and a
bearer API key.

- **Route prefix** `kilocode/` -- `kilocode/z-ai/glm-5.3-flash` reaches the
  upstream id `z-ai/glm-5.3-flash`.
- **Upstream wire protocol** OpenAI `POST /chat/completions`, streamed SSE.
- **Auth** a bearer API key from `app.kilo.ai/profile`, **or nothing at all**:
  the gateway serves free models to unauthenticated callers.
- **Dependencies** standard library only.

---

## Why this module is different: anonymous free access

Kilo Code advertises free models, and the gateway serves them to
**unauthenticated** callers, rate-limited to 200 requests per hour per IP
address.  This module models that as a real, selectable credential -- the
`anonymous` account -- so the module serves free models out of the box with no
config, no sign-in and no key:

```
kilocode/kilo-auto/free
kilocode/z-ai/glm-5.3-flash
kilocode/nvidia/nemotron-3-ultra-550b-a55b:free
```

When an operator also adds API keys, the pool **prefers a key over the anonymous
bucket** (the key carries that operator's own, higher limits), and the anonymous
identity is only used when no key is selectable.  It can be disabled from the
panel or with `"anonymous": false`, but not removed: it is rebuilt on every
start.

---

## Model ids

`Models` merges a small built-in fallback table with the live
`GET /models` catalogue.  The live response always wins; the built-in table only
covers a total outage, and it lists only the free **chat-capable** ids (the
safety-classifier and music-generation free entries are deliberately absent,
because they are not chat models and make bad probe targets).

`GET /models` does not require authentication, so a cold process can populate
the catalogue before any credential exists.

### Free detection

**Freeness is never decided from the model name.**  The rule is the union of
three independent signals carried by each live row:

1. the vendor's own `isFree` flag, or
2. the id ends with `:free` (or is `kilo-auto/free` / `openrouter/free`), or
3. the published `pricing.prompt` **and** `pricing.completion` are both `"0"`.

A vendor that publishes no pricing at all is *unknown*, not free -- the module
never guesses.

### `free_only`

Like the `openrouter` module, this deployment exists to spend free quota, so
`free_only` **defaults to true**.  It narrows the served catalogue *and* refuses
a paid id before any upstream round trip, so an explicitly named paid model
cannot reach the vendor by accident.  Set `"free_only": false` to reach the
full catalogue.

### Output limits

The module implements `core.ModelLimitsProvider`.  `ModelMaxOutputTokens`
answers from the **cached** catalogue only -- it runs inside a chat request and
must never make a metadata round trip -- and declines (`false`) on a cold cache
rather than guess.  The number is also published as
`core.Model.Extra["max_output_tokens"]`, taken from the vendor's own
`top_provider.max_completion_tokens`.

---

## Configuration

The module reads `clients.kilocode` from the host configuration.  **Every key is
optional**: an absent object is treated as `{}`, and a malformed object is logged
and replaced with defaults -- the factory logs a line containing `invalid config`
and `Status` reports not-ready rather than refusing to build.

```jsonc
{
  "clients": {
    "kilocode": {
      "base_url": "",              // default https://api.kilo.ai/api/gateway
      "app_base": "",              // default https://app.kilo.ai

      "api_key": "",               // single-account shorthand
      "accounts": [
        {"label": "personal", "api_key": "", "disabled": false}
      ],

      "anonymous": true,           // serve free models with no sign-in
      "free_only": true,           // refuse paid ids before the round trip

      "version": "",               // X-KiloCode-Version header (omit when empty)
      "mode": "",                  // default x-kilocode-mode header
      "user_agent": "",
      "extra_headers": {},
      "extra_models": [],

      "max_tokens_field": "max_tokens",   // or "max_completion_tokens"
      "max_in_flight": 4,
      "test_model": "kilo-auto/free",

      "chat_timeout": "10m",
      "models_timeout": "30s",
      "stream_idle_timeout": "90s",
      "models_ttl": "10m",
      "cooldown": "60s",
      "rate_cooldown": "2m",
      "quota_cooldown": "30m",
      "auth_cooldown": "30m"
    }
  }
}
```

### Top-level keys

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `base_url` | string | `https://api.kilo.ai/api/gateway` | inference and catalogue endpoints |
| `app_base` | string | `https://app.kilo.ai` | console the key-page link points at (display only) |
| `api_key` | string | -- | single-credential shorthand |
| `accounts` | array | -- | the multi-key roster |
| `anonymous` | bool | `true` | expose the gateway's unauthenticated free-tier identity |
| `free_only` | bool | `true` | restrict the catalogue and the chat guard to zero-cost ids |
| `version` | string | -- | `X-KiloCode-Version`; empty means "do not send it" |
| `mode` | string | -- | default `x-kilocode-mode` (a request may override it) |
| `user_agent` | string | `client2api/1.0 (+kilocode)` | outbound User-Agent |
| `extra_headers` | object | -- | extra request headers (reserved headers are refused) |
| `extra_models` | array | -- | ids appended to the built-in fallback table |
| `max_tokens_field` | string | `max_tokens` | output-cap spelling (`max_tokens` or `max_completion_tokens`) |
| `max_in_flight` | int | `4` | in-flight ceiling per credential |
| `test_model` | string | `kilo-auto/free` | id the panel's Test button probes |
| `chat_timeout` | duration | `10m` | whole chat request |
| `models_timeout` | duration | `30s` | catalogue fetch |
| `stream_idle_timeout` | duration | `90s` | gap between two SSE frames |
| `models_ttl` | duration | `10m` | how long a catalogue answer is fresh |
| `cooldown` | duration | `60s` | park after an unattributed failure |
| `rate_cooldown` | duration | `2m` | park after throttling |
| `quota_cooldown` | duration | `30m` | park after a credit refusal |
| `auth_cooldown` | duration | `30m` | park after a key rejection |

**Durations** accept a Go duration string (`"90s"`, `"10m"`).  A value that is
neither parseable nor positive falls back to the default -- a typo degrades
rather than fails.

### `accounts[]` entries

| Key | Type | Meaning |
| --- | --- | --- |
| `label` | string | shown on the panel; never a secret |
| `api_key` | string | required; a Kilo API key (a JWT) |
| `id` | string | optional stable pool id; defaults to a fingerprint of the key |
| `disabled` | bool | excluded from selection until re-enabled |

Precedence: `accounts[]` first, then the single-account shorthand, then the
environment, then imported keys, then the anonymous identity.  An entry with an
empty key is dropped rather than stored, and the same key supplied twice becomes
one account.

### Environment

| Variable | Meaning |
| --- | --- |
| `KILO_API_KEY` | a Kilo API key, when the config leaves `api_key` empty |

---

## Credential discovery

`Discover` reports, read-only:

- the module's own credential store (`data/kilocode/credentials.json`);
- `KILO_API_KEY`, when set;
- any conventional Kilo Code state files it can see -- `~/.kilocode/...`,
  `~/.kilo/...`, `%APPDATA%\kilocode\...`, and the VS Code globalStorage copy of
  the extension's `providers.json`.

Kilo Code's own extension sign-in token lives in VS Code's storage, which is not
a readable plaintext file, so auto-discovery of a signed-in extension session is
**not** claimed.  The reliable paths are the anonymous free tier, a pasted API
key, and `KILO_API_KEY`.

---

## Files in `Deps.DataDir`

| File | Contents |
| --- | --- |
| `credentials.json` | imported/panel keys (config, environment and the anonymous identity are not written here) |
| `state.json` | the health mirror: cooldown, last error, last use |

Writes go through `core.WriteJSONAtomic`.  No credential is ever written to
`state.json`, and no key is ever logged in full: log lines and error strings pass
through `core.Redact` and are additionally scrubbed of the account's own key.

---

## Account pool, cooldowns and errors

Accounts are tried least-recently-used first, **preferring a keyed credential
over the anonymous bucket**, skipping any credential the request has already
used.  The credential bound to a conversation is preferred while it is still
available, so a multi-turn exchange stays on one credential and keeps the
upstream prompt cache warm.

| Upstream condition | Kind | Park | Retry another credential |
| --- | --- | --- | --- |
| network failure, no response | server | `short` | yes |
| HTTP 402 | credit | `quota_cooldown` | yes |
| HTTP 429, body mentions a free/daily/per-hour cap | daily | until the daily reset | yes |
| HTTP 429, otherwise | rate limit | `rate_cooldown` | yes |
| HTTP 5xx | server | `short` | yes |
| HTTP 401 / 403 | auth | `auth_cooldown` | yes |
| model-not-found / content-blocked | none | no park | no |

A model that does not exist and a moderated prompt say nothing about the
credential, so neither parks the account.  A vendor refusal is surfaced through
`core.Fail("kilocode", accountID, kind, status, err)`.

---

## Streaming

- `data: [DONE]` ends the stream; a final frame without a trailing blank line is
  still delivered.
- `usage` and the terminal event are emitted at most once each.
- `Recv` returns `io.EOF` exactly once; `Close` is idempotent and releases the
  in-flight slot.
- Finish reasons normalise to `stop` / `length` / `tool_calls` / `content_filter`.
- Reasoning arrives as `delta.reasoning` and is forwarded as `Event.Reasoning`.
- Tool-call argument fragments are concatenated by index, never overwritten.
- An error member mid-stream becomes an `EventError`; an unparseable frame is
  skipped rather than fatal.

---

## Verified vs assumed

Live-verified **2026-10-11** against the real gateway, anonymously:

- `GET /api/gateway/models` returned 390 ids, of which 18 were free.
- `POST /api/gateway/chat/completions` with `kilo-auto/free` and no
  `Authorization` header returned a real completion.

Guarded, env-driven live checks ship in `kilocode_live_test.go`:

```sh
KILOCODE_LIVE=1 HTTPS_PROXY=http://<proxy> go test ./clients/kilocode/ -run Live -v
```

### Known gaps

- **No browser/PKCE login.**  Kilo API keys are created in the dashboard
  (`app.kilo.ai/profile`); a login flow that redirects back into the extension is
  not reproduced here.  Use the anonymous identity or paste a key.
- **No balance / credits surface.**  The gateway exposes no credit endpoint the
  module could read, and a fabricated `0` would be worse than saying so.
- **No check-in, task board or bundle import.**  The gateway has no such APIs.
- **`X-KiloCode-Version` is off by default.**  The gateway treats it as an
  optional hint; send it from config if a model starts requiring a client
  version.

### Account-ban risk

Automating a consumer account can violate the vendor's terms.  The anonymous
free tier is shared per IP and rate-limited; do not use it to exceed a fair-use
limit, and run keyed accounts you are willing to lose.